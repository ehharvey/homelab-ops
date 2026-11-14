package server

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
	"github.com/ehharvey/homelab-ops/internal/seedwire"
	"github.com/ehharvey/homelab-ops/internal/store"
)

// TestSeedResponseDecodesToRenderedFiles pins the seed route's JSON to the
// CLI's decoder (#242): apiclient.DecodeSeed turns the route's answer back
// into exactly the files RenderConfig rendered, in its order, so the media the
// CLI builds from a downloaded seed carries what server-side media would.
//
// The files are a real render (a real sync into the real store, then
// resolveInstanceSeed), not a fixed list: a file RenderConfig gains that
// seedwire.Fields lacks fails here, as an Encode error.
func TestSeedResponseDecodesToRenderedFiles(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tunnels := &fakeTunnelSource{}
	if _, err := SyncOnce(ctx, fakeSyncer{cfg: staticIPLessFleet(), sha: "commit1"}, st, tunnels, st, privateRepo, time.Now()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	rendered, err := resolveInstanceSeed(ctx, st, fakeCertSource{pem: sampleClientCertPEM(t)}, tunnels, st, "devnode1")
	if err != nil {
		t.Fatalf("resolveInstanceSeed: %v", err)
	}

	resp, err := seedwire.Encode(rendered.files)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := apiclient.DecodeSeed(body)
	if err != nil {
		t.Fatalf("DecodeSeed: %v", err)
	}
	if !reflect.DeepEqual(got, rendered.files) {
		t.Errorf("DecodeSeed(seedwire.Encode(files)) = %v, want %v", got, rendered.files)
	}
}
