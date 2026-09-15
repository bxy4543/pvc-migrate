package kube

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/labring-sigs/pvc-migrate/internal/domain"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// ErrVolumeUsageUnsupported indicates that a source volume is not an OpenEBS
// HostPath volume and must be handled by another usage policy.
var ErrVolumeUsageUnsupported = errors.New("volume usage backend is unsupported")

// HostPathUsageReader measures allocated filesystem bytes from a read-only PVC
// mount. HostPath has no usage field in its Kubernetes API objects, so this
// reader uses a short-lived, node-constrained Pod and never writes the volume.
type HostPathUsageReader struct {
	client  kubernetes.Interface
	image   string
	timeout time.Duration
	poll    time.Duration
}

func NewHostPathUsageReader(client kubernetes.Interface, image string) *HostPathUsageReader {
	return &HostPathUsageReader{
		client:  client,
		image:   strings.TrimSpace(image),
		timeout: 2 * time.Minute,
		poll:    250 * time.Millisecond,
	}
}

func (r *HostPathUsageReader) WithTimeout(timeout, poll time.Duration) *HostPathUsageReader {
	if r == nil {
		return r
	}
	if timeout > 0 {
		r.timeout = timeout
	}
	if poll > 0 {
		r.poll = poll
	}
	return r
}

func (r *HostPathUsageReader) Read(
	ctx context.Context,
	options VolumeUsageReadOptions,
) (result VolumeUsageReadResult, retErr error) {
	if r == nil || r.client == nil {
		return VolumeUsageReadResult{}, domain.NewError(
			domain.ErrorInternal,
			"hostpath usage",
			"Kubernetes client is required",
		)
	}
	if options.SourcePVC.Namespace == "" || options.SourcePVC.Name == "" ||
		options.SourcePVC.UID == "" || options.SourcePV.Name == "" || options.SourcePV.UID == "" {
		return VolumeUsageReadResult{}, domain.NewError(
			domain.ErrorValidation,
			"hostpath usage",
			"source PVC and PV names and UIDs are required",
		)
	}
	if r.image == "" {
		return VolumeUsageReadResult{}, domain.NewError(
			domain.ErrorPrecondition,
			"hostpath usage",
			"trusted tool image is required",
		)
	}

	pvc, err := r.client.CoreV1().PersistentVolumeClaims(options.SourcePVC.Namespace).
		Get(ctx, options.SourcePVC.Name, metav1.GetOptions{})
	if err != nil {
		return VolumeUsageReadResult{}, err
	}
	if pvc.UID != options.SourcePVC.UID || pvc.Spec.VolumeName != options.SourcePV.Name {
		return VolumeUsageReadResult{}, domain.NewError(
			domain.ErrorConflict,
			"hostpath usage",
			"source PVC identity or binding changed",
		)
	}

	pv, err := r.client.CoreV1().PersistentVolumes().
		Get(ctx, options.SourcePV.Name, metav1.GetOptions{})
	if err != nil {
		return VolumeUsageReadResult{}, err
	}
	if pv.UID != options.SourcePV.UID || pv.Spec.ClaimRef == nil ||
		pv.Spec.ClaimRef.Namespace != pvc.Namespace ||
		pv.Spec.ClaimRef.Name != pvc.Name || pv.Spec.ClaimRef.UID != pvc.UID {
		return VolumeUsageReadResult{}, domain.NewError(
			domain.ErrorConflict,
			"hostpath usage",
			"source PV identity or claimRef changed",
		)
	}

	if !IsHostPathPersistentVolume(pv) {
		className := ""
		if pvc.Spec.StorageClassName != nil {
			className = *pvc.Spec.StorageClassName
		}
		if className == "" {
			return VolumeUsageReadResult{}, ErrVolumeUsageUnsupported
		}
		sc, getErr := r.client.StorageV1().StorageClasses().Get(ctx, className, metav1.GetOptions{})
		if getErr != nil {
			return VolumeUsageReadResult{}, getErr
		}
		if !IsOpenEBSHostPathStorageClass(sc) {
			return VolumeUsageReadResult{}, ErrVolumeUsageUnsupported
		}
	}

	if IsHostPathPersistentVolume(pv) && (pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil) {
		return VolumeUsageReadResult{}, domain.NewError(
			domain.ErrorPrecondition,
			"hostpath usage",
			"static HostPath PV has no node affinity; usage cannot be measured on a verified volume node",
		)
	}

	name := BoundedName(
		"pvc-migrate-usage",
		string(options.SourcePVC.UID),
		string(options.SourcePV.UID),
		string(uuid.NewUUID()),
	)
	pod := hostPathUsagePod(r.image, name, pvc.Namespace, pvc.Name, options.OperationID, pv)
	created, err := r.client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return VolumeUsageReadResult{}, domain.WrapError(
			domain.ErrorPrecondition,
			"hostpath usage",
			fmt.Sprintf("create usage Pod %s/%s", pod.Namespace, pod.Name),
			err,
		)
	}
	defer func() {
		retErr = errors.Join(retErr, r.cleanup(created.Namespace, created.Name, created.UID))
	}()

	waitCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var terminationMessage string
	err = WaitFor(
		waitCtx,
		r.poll,
		fmt.Sprintf("HostPath usage Pod %s/%s", created.Namespace, created.Name),
		func(checkCtx context.Context) (bool, error) {
			current, getErr := r.client.CoreV1().Pods(created.Namespace).
				Get(checkCtx, created.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(getErr) {
				return false, domain.NewError(
					domain.ErrorConflict,
					"hostpath usage",
					"usage Pod disappeared before completion",
				)
			}
			if getErr != nil {
				return false, getErr
			}
			if current.UID != created.UID {
				return false, domain.NewError(
					domain.ErrorConflict,
					"hostpath usage",
					"usage Pod was replaced before completion",
				)
			}
			switch current.Status.Phase {
			case corev1.PodSucceeded:
				if len(current.Status.ContainerStatuses) == 0 ||
					current.Status.ContainerStatuses[0].State.Terminated == nil {
					return false, domain.NewError(
						domain.ErrorPrecondition,
						"hostpath usage",
						"usage Pod completed without a termination message",
					)
				}
				terminationMessage = strings.TrimSpace(
					current.Status.ContainerStatuses[0].State.Terminated.Message,
				)
				return true, nil
			case corev1.PodFailed:
				return false, domain.NewError(
					domain.ErrorPrecondition,
					"hostpath usage",
					fmt.Sprintf("usage Pod failed: %s %s", current.Status.Reason, current.Status.Message),
				)
			default:
				return false, nil
			}
		},
	)
	if err != nil {
		return VolumeUsageReadResult{}, err
	}

	used, err := strconv.ParseInt(terminationMessage, 10, 64)
	if err != nil || used < 0 {
		return VolumeUsageReadResult{}, domain.NewError(
			domain.ErrorPrecondition,
			"hostpath usage",
			fmt.Sprintf("usage Pod returned invalid used bytes %q", terminationMessage),
		)
	}

	result = VolumeUsageReadResult{
		UsedBytes: used,
		Source:    "read-only HostPath usage Pod",
	}
	return result, nil
}

func (r *HostPathUsageReader) cleanup(namespace, name string, uid types.UID) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.client.CoreV1().Pods(namespace).Delete(
		cleanupCtx,
		name,
		metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}},
	)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return domain.WrapError(domain.ErrorKubernetes, "hostpath usage cleanup", "delete usage Pod", err)
	}
	return WaitFor(cleanupCtx, r.poll, fmt.Sprintf("HostPath usage Pod %s/%s deletion", namespace, name), func(waitCtx context.Context) (bool, error) {
		current, getErr := r.client.CoreV1().Pods(namespace).Get(waitCtx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		if getErr != nil {
			return false, getErr
		}
		if current.UID != uid {
			return false, domain.NewError(domain.ErrorConflict, "hostpath usage cleanup", "usage Pod was replaced during cleanup")
		}
		return false, nil
	})
}

func hostPathUsagePod(
	image, name, namespace, pvcName, operationID string,
	pv *corev1.PersistentVolume,
) *corev1.Pod {
	deadline := int64(120)
	automount := false
	root := int64(0)
	allowPrivilegeEscalation := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				ManagedByLabel:    ManagedByValue,
				ResourceRoleLabel: ResourceRoleToolProbe,
			},
			Annotations: map[string]string{MetadataDomain + "/usage-pvc": pvcName},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        &deadline,
			AutomountServiceAccountToken: &automount,
			Containers: []corev1.Container{{
				Name:            "usage",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{
					"sh",
					"-c",
					"allocated=$(du -sk -x /probe-volume | awk 'NR==1 {printf \"%.0f\", $1 * 1024}'); apparent=$(du -sb -x /probe-volume | awk 'NR==1 {printf \"%.0f\", $1}'); if [ \"$allocated\" -ge \"$apparent\" ]; then printf '%s' \"$allocated\"; else printf '%s' \"$apparent\"; fi > /dev/termination-log",
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                &root,
					RunAsGroup:               &root,
					AllowPrivilegeEscalation: &allowPrivilegeEscalation,
					Capabilities: &corev1.Capabilities{
						Add:  []corev1.Capability{"DAC_READ_SEARCH"},
						Drop: []corev1.Capability{"ALL"},
					},
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "source-pvc",
					MountPath: "/probe-volume",
					ReadOnly:  true,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "source-pvc",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
					ReadOnly:  true,
				}},
			}},
		},
	}
	if operationID != "" {
		pod.Labels[SessionKey] = operationID
	}
	if pv != nil && pv.Spec.NodeAffinity != nil && pv.Spec.NodeAffinity.Required != nil {
		pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: pv.Spec.NodeAffinity.Required.DeepCopy(),
		}}
	}
	return pod
}

func IsOpenEBSHostPathStorageClass(sc *storagev1.StorageClass) bool {
	if sc == nil || sc.Provisioner != OpenEBSLocalPVProvisioner {
		return false
	}
	for key, value := range sc.Parameters {
		if strings.EqualFold(key, "storageType") && strings.EqualFold(strings.TrimSpace(value), "hostpath") {
			return true
		}
	}
	var entries []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	}
	if err := yaml.Unmarshal([]byte(sc.Annotations["cas.openebs.io/config"]), &entries); err != nil {
		return false
	}
	for _, entry := range entries {
		if strings.EqualFold(strings.TrimSpace(entry.Name), "StorageType") &&
			strings.EqualFold(strings.TrimSpace(entry.Value), "hostpath") {
			return true
		}
	}
	return false
}

func IsHostPathPersistentVolume(pv *corev1.PersistentVolume) bool {
	return pv != nil && pv.Spec.HostPath != nil
}

var _ FilesystemUsageReader = (*HostPathUsageReader)(nil)
