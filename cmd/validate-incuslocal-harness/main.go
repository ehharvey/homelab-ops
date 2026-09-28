// Command validate-incuslocal-harness is test-only scaffolding for
// scripts/validate/incuslocal-round-trips-instances-over-unix-socket.sh — it
// is NOT an operator-facing command like cmd/bootstrap or cmd/web, and ships
// no stability promise. Same role, and the same reasoning, as
// cmd/validate-tunnel-harness: the thing under test is a Go package, so
// proving it against a real Incus unix socket needs a Go process the script
// can drive.
//
// Deliberately thin. Every mode is one internal/incuslocal call and a printed
// result, with no logic of its own — if the harness decided anything, a green
// script would stop being evidence about the package. Setup and teardown
// (creating fixtures, pushing a file into a container, checking an instance
// really went away) are the script's job, done with the incus CLI, so the
// package under test is never both the actor and the oracle.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	lxcapi "github.com/lxc/incus/v7/shared/api"

	"github.com/ehharvey/homelab-ops/internal/incuslocal"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "validate-incuslocal-harness:", err)
		os.Exit(1)
	}
}

func run() error {
	mode := flag.String("mode", "", "list | create | set-user-keys | read-file | delete")
	socket := flag.String("socket", "/var/lib/incus/unix.socket", "path to the Incus unix socket")
	instanceName := flag.String("instance-name", "", "instance to act on")
	appTag := flag.String("app-tag", "", "value for "+incuslocal.AppTagKey+" on a created instance; empty leaves the instance untagged, which is how the script proves ListInstances excludes unmanaged workloads")
	storagePool := flag.String("storage-pool", "", "attach a root disk on this pool when creating; the script passes a name that does not exist to prove a create Incus accepts and then fails is reported as an error (#161)")
	keys := flag.String("keys", "", "comma-separated k=v pairs for set-user-keys")
	path := flag.String("path", "", "file path inside the instance, for read-file")
	timeout := flag.Duration("timeout", 30*time.Second, "bound on the whole operation")
	flag.Parse()

	client, err := incuslocal.Dial(*socket)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	switch *mode {
	case "list":
		return runList(ctx, client)
	case "create":
		return runCreate(ctx, client, *instanceName, *appTag, *storagePool)
	case "set-user-keys":
		return runSetUserKeys(ctx, client, *instanceName, *keys)
	case "read-file":
		return runReadFile(ctx, client, *instanceName, *path)
	case "delete":
		return client.DeleteInstance(ctx, *instanceName)
	default:
		return fmt.Errorf("unknown -mode %q (want list, create, set-user-keys, read-file, or delete)", *mode)
	}
}

// runList prints ListInstances as JSON, so the script can assert against it
// with jq rather than parsing prose. incuslocal.Instance carries no json
// tags, so the keys are the Go field names verbatim (Name, Status, Location,
// Config) — the script's jq filters depend on that, and both files are
// test-only, so the coupling stays inside the harness/script pair.
func runList(ctx context.Context, client *incuslocal.Client) error {
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return err
	}
	out, err := json.Marshal(instances)
	if err != nil {
		return fmt.Errorf("marshal listing: %w", err)
	}
	fmt.Println(string(out))
	return nil
}

func runCreate(ctx context.Context, client *incuslocal.Client, name, appTag, storagePool string) error {
	req := lxcapi.InstancesPost{
		Name: name,
		Type: lxcapi.InstanceTypeContainer,
		// source type "none" creates an instance record with no image, which
		// is instant and needs nothing cached — the same trick
		// incus-etag-write-guards-lost-updates-not-races.sh uses.
		Source: lxcapi.InstanceSource{Type: "none"},
	}
	if appTag != "" {
		req.Config = map[string]string{incuslocal.AppTagKey: appTag}
	}
	if storagePool != "" {
		req.Devices = map[string]map[string]string{
			"root": {"type": "disk", "pool": storagePool, "path": "/"},
		}
	}
	if err := client.CreateInstance(ctx, req); err != nil {
		return err
	}
	fmt.Printf("created %s\n", name)
	return nil
}

func runSetUserKeys(ctx context.Context, client *incuslocal.Client, name, keys string) error {
	if keys == "" {
		return fmt.Errorf("-keys is required for set-user-keys")
	}
	parsed := map[string]string{}
	for _, pair := range strings.Split(keys, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return fmt.Errorf("malformed -keys entry %q, want k=v", pair)
		}
		parsed[k] = v
	}
	if err := client.SetUserKeys(ctx, name, parsed); err != nil {
		return err
	}
	fmt.Printf("set %d key(s) on %s\n", len(parsed), name)
	return nil
}

// runReadFile writes the file's bytes to stdout verbatim, so the script can
// compare them against what it pushed in.
func runReadFile(ctx context.Context, client *incuslocal.Client, name, path string) error {
	content, err := client.ReadFile(ctx, name, path)
	if err != nil {
		return err
	}
	if _, err := os.Stdout.Write(content); err != nil {
		return fmt.Errorf("write content to stdout: %w", err)
	}
	return nil
}
