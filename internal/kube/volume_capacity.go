package kube

import (
	"math"

	"k8s.io/apimachinery/pkg/api/resource"
)

const hostPathCapacityUnit int64 = 1024 * 1024

// HostPathDestinationCapacity returns a conservative destination request for
// measured filesystem usage. It adds 20 percent, rounds up to MiB, and never
// shrinks below the source PV's declared capacity.
func HostPathDestinationCapacity(source resource.Quantity, usedBytes int64) resource.Quantity {
	if usedBytes < 0 {
		return source
	}

	margin := int64(0)
	if usedBytes > math.MaxInt64-4 {
		margin = math.MaxInt64 - usedBytes
	} else {
		margin = (usedBytes + 4) / 5
	}
	required := usedBytes + margin
	if required < usedBytes {
		required = math.MaxInt64
	}
	if remainder := required % hostPathCapacityUnit; remainder != 0 && required <= math.MaxInt64-(hostPathCapacityUnit-remainder) {
		required += hostPathCapacityUnit - remainder
	}

	measured := *resource.NewQuantity(required, resource.DecimalSI)
	if measured.Cmp(source) < 0 {
		return source
	}
	return measured
}
