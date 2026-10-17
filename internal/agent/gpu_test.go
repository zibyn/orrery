package agent

import "testing"

func TestParseGPUList(t *testing.T) {
	out := `GPU-0f1e2d3c-aaaa-bbbb-cccc-000000000001, NVIDIA H100 80GB HBM3
MIG-GPU-0f1e2d3c-aaaa-bbbb-cccc-000000000001/1/0, NVIDIA H100 80GB HBM3
GPU-0f1e2d3c-aaaa-bbbb-cccc-000000000002, NVIDIA A100-SXM4-80GB
`
	gpus := parseGPUList(out)
	if len(gpus) != 2 {
		t.Fatalf("parseGPUList returned %d GPUs (%+v), want 2 whole cards", len(gpus), gpus)
	}
	if gpus[0].UUID != "GPU-0f1e2d3c-aaaa-bbbb-cccc-000000000001" || gpus[0].Model != "NVIDIA H100 80GB HBM3" {
		t.Fatalf("first GPU = %+v", gpus[0])
	}
	if gpus[1].Model != "NVIDIA A100-SXM4-80GB" {
		t.Fatalf("second GPU = %+v", gpus[1])
	}

	if gpus := parseGPUList("no cards\n"); len(gpus) != 0 {
		t.Fatalf("parseGPUList(no cards) = %+v", gpus)
	}
}
