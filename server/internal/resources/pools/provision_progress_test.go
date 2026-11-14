package pools

import (
	"encoding/json"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A pool's progress is stored as the JSON its driver reports
// (ReportPoolProvisionProgress) and served as the API's PoolProvisionProgress,
// whose decoder refuses a field it does not know. Every field a driver can
// report reaches a client only if the two agree on its name and shape.
func TestStoredPoolProgressDecodesAsTheAPIsShape(t *testing.T) {
	progress := sandbox.PoolProvisionProgress{
		Phase: sandbox.PoolPhaseFetchingVMImage,
		Pull: &sandbox.PoolPullProgress{
			Image:           "ghcr.io/discobox-ai/discobox-vm:v1",
			Current:         120 << 20,
			Total:           400 << 20,
			Layers:          9,
			LayersComplete:  2,
			Extracted:       64 << 20,
			LayersExtracted: 1,
			Retry:           &sandbox.PoolPullRetry{Layer: 3, Attempt: 2, Kept: 172 << 20},
			Done:            true,
		},
	}
	encoded, err := json.Marshal(progress)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var served serverapi.PoolProvisionProgress
	if err := served.UnmarshalJSON(encoded); err != nil {
		t.Fatalf("the API refuses the stored progress %s: %v", encoded, err)
	}
	if served.Phase != serverapi.PoolProvisionPhaseFetchingVMImage {
		t.Errorf("served phase = %q", served.Phase)
	}
	pull, ok := served.Pull.Get()
	if !ok || pull.Image != progress.Pull.Image || pull.Current.Or(0) != 120<<20 || pull.Total.Or(0) != 400<<20 ||
		pull.Layers.Or(0) != 9 || pull.LayersComplete.Or(0) != 2 || pull.Extracted.Or(0) != 64<<20 ||
		pull.LayersExtracted.Or(0) != 1 || !pull.Done.Or(false) {
		t.Errorf("served pull = %+v, want every count the driver stored", pull)
	}
	retry, ok := pull.Retry.Get()
	if !ok || retry.Layer != 3 || retry.Attempt.Or(0) != 2 || retry.Kept.Or(0) != 172<<20 {
		t.Errorf("served retry = %+v (set %v), want layer 3, attempt 2, 172 MiB kept", retry, ok)
	}
}
