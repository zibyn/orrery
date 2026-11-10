package api

import (
	"reflect"
	"testing"

	"github.com/zibyn/orrery/internal/store"
)

// The pools on GET /capacity are the ones admission enforces (ADR-0015): when a
// pool shows nothing left, the matching admits check refuses one more unit.
func TestCapacityPools(t *testing.T) {
	const gib = int64(1) << 30

	t.Run("GPU Node splits CPU and memory into two pools", func(t *testing.T) {
		node := store.Node{
			CpuPerGpu: 1.5, MemoryPerGpu: 6 * gib,
			Hardware: mustHardware(t, nodeHardware{
				CPUCores: 8, MemoryBytes: 32 * gib, DiskFree: 40, DiskTotal: 100,
			}),
		}
		// One 1-card Sandbox (1.5 CPU, 6 GiB) and one GPU-less (1 CPU, 2 GiB).
		used := store.NodePlacementRow{
			GpuTotal: 4, UsedCpu: 2.5, UsedMemory: 8 * gib,
			UsedCpuNoGpu: 1, UsedMemoryNoGpu: 2 * gib,
		}
		c := capacityFor(node, used)
		wantCPU := &capacityPools{GPUPool: &capacityPool{Total: 6, Allocated: 1.5}, GeneralPool: capacityPool{Total: 2, Allocated: 1}}
		wantMem := &capacityPools{
			GPUPool:     &capacityPool{Total: float64(24 * gib), Allocated: float64(6 * gib)},
			GeneralPool: capacityPool{Total: float64(8 * gib), Allocated: float64(2 * gib)},
		}
		if !reflect.DeepEqual(c.CPU, wantCPU) || !reflect.DeepEqual(c.Memory, wantMem) {
			t.Fatalf("cpu = %+v, memory = %+v", c.CPU, c.Memory)
		}
		if want := (&capacityDisk{Total: 100, Free: 40, Watermark: defaultDiskWatermark}); !reflect.DeepEqual(c.Disk, want) {
			t.Fatalf("disk = %+v, want %+v (default watermark)", c.Disk, want)
		}

		fit, _ := fitFor(node, used)
		// The general pool has 1 CPU and 6 GiB left: exactly that fits.
		if !fit.admitsNonGPU(1, 6*gib) || fit.admitsNonGPU(1.1, 1) || fit.admitsNonGPU(0.1, 6*gib+1) {
			t.Fatal("admitsNonGPU disagrees with the general pool's remainder")
		}
		// The GPU pool has 4.5 CPU and 18 GiB left, the share of the 3 free cards.
		if !fit.admitsGPU(4.5, 18*gib, 3) {
			t.Fatal("admitsGPU refused the GPU pool's remainder")
		}
		// One unit past the share of the 3 free cards does not fit.
		if fit.admitsGPU(4.6, 18*gib, 3) || fit.admitsGPU(4.5, 18*gib+1, 3) {
			t.Fatal("admitsGPU admitted more than the GPU pool's remainder")
		}
	})

	t.Run("GPU-less Node has only the general pool", func(t *testing.T) {
		node := store.Node{
			ReservedCpu: 1, ReservedMemory: 2 * gib,
			Hardware: mustHardware(t, nodeHardware{
				CPUCores: 8, MemoryBytes: 32 * gib, DiskFree: 50, DiskTotal: 100, DiskWatermark: 0.2,
			}),
		}
		used := store.NodePlacementRow{UsedCpu: 6, UsedMemory: 20 * gib, UsedCpuNoGpu: 6, UsedMemoryNoGpu: 20 * gib}
		c := capacityFor(node, used)
		wantCPU := &capacityPools{GeneralPool: capacityPool{Total: 7, Allocated: 6}}
		wantMem := &capacityPools{GeneralPool: capacityPool{Total: float64(30 * gib), Allocated: float64(20 * gib)}}
		if !reflect.DeepEqual(c.CPU, wantCPU) || !reflect.DeepEqual(c.Memory, wantMem) {
			t.Fatalf("cpu = %+v, memory = %+v", c.CPU, c.Memory)
		}
		if c.Disk == nil || c.Disk.Watermark != 0.2 {
			t.Fatalf("disk = %+v, want the Node's 0.2 watermark", c.Disk)
		}
		fit, _ := fitFor(node, used)
		if !fit.admitsNonGPU(1, 10*gib) || fit.admitsNonGPU(1.1, 1) {
			t.Fatal("admitsNonGPU disagrees with the general pool's remainder")
		}
	})

	t.Run("full pool shows nothing left and admits nothing more", func(t *testing.T) {
		node := store.Node{
			Hardware: mustHardware(t, nodeHardware{CPUCores: 4, MemoryBytes: 8 * gib, DiskFree: 50, DiskTotal: 100}),
		}
		used := store.NodePlacementRow{UsedCpu: 4, UsedMemory: 8 * gib, UsedCpuNoGpu: 4, UsedMemoryNoGpu: 8 * gib}
		c := capacityFor(node, used)
		if left := c.CPU.GeneralPool.Total - c.CPU.GeneralPool.Allocated; left != 0 {
			t.Fatalf("cpu left = %v, want 0", left)
		}
		if left := c.Memory.GeneralPool.Total - c.Memory.GeneralPool.Allocated; left != 0 {
			t.Fatalf("memory left = %v, want 0", left)
		}
		fit, _ := fitFor(node, used)
		if fit.admitsNonGPU(1, 0) || fit.admitsNonGPU(0, 1) {
			t.Fatal("admitsNonGPU admitted a unit into a full pool")
		}
	})

	t.Run("Node without reported hardware has no CPU, memory or disk", func(t *testing.T) {
		c := capacityFor(store.Node{}, store.NodePlacementRow{GpuTotal: 2})
		if c.CPU != nil || c.Memory != nil || c.Disk != nil {
			t.Fatalf("capacity = %+v, want all null", c)
		}
	})
}
