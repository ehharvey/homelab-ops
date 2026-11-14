package apiclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ehharvey/homelab-ops/internal/webtls"
)

// TLSOptions is how PinnedHTTPClient talks TLS to the web app (#397).
type TLSOptions struct {
	// ClientCert is the operator cert presented to the web app, or nil to
	// present none (the web app then answers every request 403).
	ClientCert *tls.Certificate
	// ServerPin is the fingerprint (webtls.Fingerprint) the web app's TLS
	// key must have. Required: there is no CA to verify the server against.
	ServerPin string
	// PinSource says where ServerPin came from, for a mismatch's error: a
	// flag's name, e.g. "--server-fingerprint", or Pinning.Resolve's source.
	PinSource string
}

// LoadClientCert loads the operator cert's two PEM halves for
// TLSOptions.ClientCert. Both empty is no cert (nil); one without the other
// is an error.
func LoadClientCert(certFile, keyFile string) (*tls.Certificate, error) {
	switch {
	case certFile == "" && keyFile == "":
		return nil, nil //nolint:nilnil // no cert is a valid answer: TLSOptions.ClientCert's nil
	case certFile == "" || keyFile == "":
		return nil, errors.New("the client cert and its key go together: give both or neither")
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &pair, nil
}

// PinnedHTTPClient returns an HTTPS client that dials every connection with
// dial, e.g. through the operator's WireGuard tunnel, presents
// opts.ClientCert, and accepts the server only if its TLS public key has
// opts.ServerPin's fingerprint. It sets no overall timeout: an image
// download may run for minutes, and the caller's context bounds a request
// instead. The web app refuses a client cert it doesn't list with an HTTP
// 403 (webtls.RequireOperator), which reaches the caller as an *APIError
// like any other answer.
func PinnedHTTPClient(dial DialFunc, opts TLSOptions) (*http.Client, error) {
	pin, err := webtls.ParseFingerprint(opts.ServerPin)
	if err != nil {
		return nil, fmt.Errorf("server pin: %w", err)
	}
	cfg := pinnedConfig(func(got string) error {
		if got != pin {
			return &PinMismatchError{Got: got, Want: pin, Source: opts.PinSource}
		}
		return nil
	})
	if opts.ClientCert != nil {
		cfg.Certificates = []tls.Certificate{*opts.ClientCert}
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: dial,
		// One operator, one request per CLI run: nothing to pool.
		DisableKeepAlives: true,
		// No system proxy: the tunnel address is reachable only through dial.
		Proxy:           nil,
		TLSClientConfig: cfg,
	}}, nil
}

// pinnedConfig is a client TLS config that checks the server by its public
// key's fingerprint alone, through check, instead of a CA chain.
//
// The web app's cert is self-signed (until step-ca, #506) and re-made at
// every start, so there is no chain or name to verify, and Go's chain check
// has to be off for the handshake to reach the key. The server is still
// verified on every handshake: VerifyConnection runs whatever
// InsecureSkipVerify says, on a resumed session too (unlike
// VerifyPeerCertificate, gosec G123), and fails the handshake unless check
// accepts the leaf's public-key fingerprint. PinnedHTTPClient's check is an
// exact match against a required pin; ServerFingerprint's only reads the key
// and aborts, sending nothing. pin_test.go proves a wrong key is refused.
//
// CodeQL's go/disabled-certificate-check flags the InsecureSkipVerify line.
// It is deliberate: this is trust-on-first-use pinning of a self-signed key,
// which a CA chain check can't express. As with internal/nodeprovision's (see
// its comment), an inline codeql[...] suppression isn't honoured for Go's
// default suite, so dismissing the alert as a false positive is done in
// GitHub's UI: the pin check replaces the chain check rather than dropping
// it.
func pinnedConfig(check func(fingerprint string) error) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Off only so the handshake reaches VerifyConnection below, which
		// verifies the server instead, on every handshake: its public key
		// must have the pinned fingerprint (operator-confirmed on first use,
		// or given as --server-fingerprint), or the handshake fails.
		InsecureSkipVerify: true, //nolint:gosec // G402: VerifyConnection pins the server's public key instead (see above)
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the web app presented no TLS certificate")
			}
			return check(webtls.Fingerprint(cs.PeerCertificates[0].RawSubjectPublicKeyInfo))
		},
	}
}

// PinMismatchError is a web app whose TLS key isn't the pinned one.
type PinMismatchError struct {
	Got, Want string
	// Source is where Want came from: a flag's name, or "recorded in
	// <file>" (Pinning.Resolve).
	Source string
}

func (e *PinMismatchError) Error() string {
	from, fix := "", "pin the new one"
	switch {
	case strings.HasPrefix(e.Source, recordedIn):
		from, fix = " ("+e.Source+")", "run again with --repin, which asks you to confirm it and replaces the recorded pin"
	case e.Source != "":
		from, fix = " ("+e.Source+")", "pass the new one as "+e.Source
	}
	return fmt.Sprintf("refusing the web app: its TLS key is %s, not the pinned %s%s. If its key changed on purpose, check the new one against its startup log (\"TLS server key fingerprint\"), then %s", e.Got, e.Want, from, fix)
}

// recordedIn starts a pin's source when it came from the pins file.
const recordedIn = "recorded in "

// errProbed ends ServerFingerprint's handshake once it has the key.
var errProbed = errors.New("probed")

// ServerFingerprint dials address and starts a TLS handshake only to read the
// server's public-key fingerprint; it presents no client cert, and sends no
// request. Pinning runs it only while there is no pin to check against.
func ServerFingerprint(ctx context.Context, dial DialFunc, address string) (string, error) {
	conn, err := dial(ctx, "tcp", address)
	if err != nil {
		return "", err
	}
	defer conn.Close() //nolint:errcheck // a probe
	var fp string
	tc := tls.Client(conn, pinnedConfig(func(got string) error {
		fp = got
		return errProbed
	}))
	err = tc.HandshakeContext(ctx)
	if fp != "" {
		return fp, nil
	}
	if err == nil {
		err = errors.New("the handshake finished without a certificate")
	}
	return "", fmt.Errorf("read the web app's TLS key: %w", err)
}

// Pinning decides which fingerprint to pin the web app's TLS key to (#397):
// trust on first use, confirmed by the operator, then the recorded one.
type Pinning struct {
	// Expected, when set, is the only fingerprint accepted, and nothing is
	// recorded or asked: how a script pins (`--server-fingerprint`).
	// PinnedHTTPClient checks its form.
	Expected string
	// File records pins, one "<server> <fingerprint>" per line.
	File string
	// Server is what the pin is recorded under: the web app's WireGuard
	// public key, which names it across restarts and host moves.
	Server string
	// Repin ignores the pin recorded for Server and asks afresh, as on first
	// use; a confirmed key replaces the recorded one (`--repin`).
	Repin bool
	// Probe reads the web app's current fingerprint (ServerFingerprint). It
	// runs only when no pin is known yet: with one, PinnedHTTPClient's own
	// handshake checks it, so a call costs no extra round trip.
	Probe func(ctx context.Context) (string, error)
	// Confirm asks the operator whether to trust a fingerprint; previous is
	// the recorded pin it would replace (Repin), or "". Nil means no one can
	// be asked (not a terminal), so an unpinned key is refused rather than
	// trusted.
	Confirm func(ctx context.Context, fingerprint, previous string) (bool, error)
}

// Resolve returns the fingerprint to pin, with where the trust came from:
//
//   - Expected set: Expected.
//   - a pin recorded for Server, unless Repin: that pin. A malformed line
//     for Server is an error unless Repin, which replaces it.
//   - otherwise Probe reads the key and Confirm asks; on yes the fingerprint
//     is recorded, replacing any earlier one for Server. With no Confirm, or
//     on no, Resolve refuses.
//
// A known pin is returned without touching the network: whether the web app
// still has that key is PinnedHTTPClient's check, on the real request.
func (p Pinning) Resolve(ctx context.Context) (pin, source string, err error) {
	if p.Expected != "" {
		return p.Expected, "--server-fingerprint", nil
	}
	recorded, ok, err := LookupPin(p.File, p.Server)
	// --repin replaces a malformed line for Server as it would a pin, so it
	// is how to recover from one without editing the file.
	if err != nil && (!p.Repin || !errors.Is(err, ErrMalformedPin)) {
		return "", "", err
	}
	if ok && !p.Repin {
		return recorded, recordedIn + p.File, nil
	}

	got, err := p.Probe(ctx)
	if err != nil {
		return "", "", err
	}
	if ok && got == recorded {
		// --repin, but the key hasn't changed: nothing to replace.
		return recorded, recordedIn + p.File, nil
	}
	if p.Confirm == nil && ok {
		return "", "", fmt.Errorf("--repin needs a terminal to confirm the web app's new TLS key %s; without one, pass it as --server-fingerprint instead", got)
	}
	if p.Confirm == nil {
		return "", "", fmt.Errorf("no pinned TLS key for this web app, and no terminal to confirm one: its key is %s. Check it against the web app's startup log (\"TLS server key fingerprint\"), then pass --server-fingerprint %s, or run once interactively to record it in %s", got, got, p.File)
	}
	yes, err := p.Confirm(ctx, got, recorded)
	if err != nil {
		return "", "", err
	}
	if !yes {
		return "", "", fmt.Errorf("the web app's TLS key %s was not confirmed; nothing recorded", got)
	}
	if err := RecordPin(p.File, p.Server, got); err != nil {
		return "", "", err
	}
	return got, recordedIn + p.File, nil
}

// PromptConfirm is a Pinning.Confirm that shows the fingerprint on out and
// reads a y/n answer from in. Only "y" or "yes" trusts it. A cancelled ctx
// (Ctrl-C) ends the wait at once; the read itself can't be interrupted, so
// it is left to the process's exit.
func PromptConfirm(in io.Reader, out io.Writer, file string) func(ctx context.Context, fp, previous string) (bool, error) {
	return func(ctx context.Context, fp, previous string) (bool, error) {
		situation := "This is the first connection to it from here."
		if previous != "" {
			situation = "It would replace the recorded " + previous + "."
		}
		_, _ = fmt.Fprintf(out, "The web app's TLS server key is %s\n"+
			"%s Check it against the\n"+
			"\"TLS server key fingerprint\" line in the web app's startup log.\n"+
			"Trust it, and record it in %s? [y/N] ", fp, situation, file)
		type answer struct {
			line string
			err  error
		}
		answered := make(chan answer, 1)
		go func() {
			line, err := bufio.NewReader(in).ReadString('\n')
			answered <- answer{line, err}
		}()
		var a answer
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintln(out)
			return false, fmt.Errorf("asking to trust the web app's TLS key: %w", ctx.Err())
		case a = <-answered:
		}
		if a.err != nil && !errors.Is(a.err, io.EOF) {
			return false, fmt.Errorf("read the answer: %w", a.err)
		}
		switch strings.ToLower(strings.TrimSpace(a.line)) {
		case "y", "yes":
			return true, nil
		default:
			return false, nil
		}
	}
}

// pinLines splits a pins file into its lines, dropping the newline after
// the last one, whether or not it has one.
func pinLines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// pinFields is a pins-file line's fields, without its # comment.
func pinFields(line string) []string {
	line, _, _ = strings.Cut(line, "#")
	return strings.Fields(line)
}

// ErrMalformedPin is a pins-file line for the server being looked up that
// isn't "<server> <fingerprint>". Lines for other servers are left unread
// beyond their first field, so a bad one can't block every web app.
var ErrMalformedPin = errors.New("malformed pin")

// LookupPin returns the fingerprint recorded for server in file: the first
// line whose first field is server. A missing file records nothing. Lines
// for other servers are skipped whatever they hold; a malformed line for
// server is an ErrMalformedPin, which Pinning's Repin replaces.
func LookupPin(file, server string) (string, bool, error) {
	b, err := os.ReadFile(file) //nolint:gosec // G304: the pins file is operator-supplied (--pins-file) or the user's config dir
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read pins: %w", err)
	}
	for n, line := range pinLines(b) {
		fields := pinFields(line)
		if len(fields) == 0 || fields[0] != server {
			continue
		}
		if len(fields) != 2 {
			return "", false, fmt.Errorf("%s:%d: %w: want \"<server> <fingerprint>\" (--repin replaces it)", file, n+1, ErrMalformedPin)
		}
		pin, err := webtls.ParseFingerprint(fields[1])
		if err != nil {
			return "", false, fmt.Errorf("%s:%d: %w: %w (--repin replaces it)", file, n+1, ErrMalformedPin, err)
		}
		return pin, true, nil
	}
	return "", false, nil
}

// RecordPin records server's fingerprint in file, replacing every line
// already recorded for server, malformed or not, and keeping every other
// line as it is, and creates the file (0600) and its directory (0700) if
// needed. It writes a temporary file and renames it over file, so a failed
// write leaves the old pins, and the result ends in a newline whether or not
// the old file did.
//
// A symlinked file is followed, so the rename replaces its target and the
// link stays. The read-modify-write holds an exclusive lock on a sibling
// "<file>.lock" (flock), so two runs recording pins at once both land.
func RecordPin(file, server, pin string) error {
	target, err := followSymlinks(file)
	if err != nil {
		return fmt.Errorf("record pin: %w", err)
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("record pin: %w", err)
	}
	unlock, err := lockFile(target + ".lock")
	if err != nil {
		return fmt.Errorf("record pin: lock %s: %w", target+".lock", err)
	}
	defer unlock()

	old, err := os.ReadFile(target) //nolint:gosec // G304: as LookupPin
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("record pin: %w", err)
	}
	var b strings.Builder
	for _, line := range pinLines(old) {
		if f := pinFields(line); len(f) > 0 && f[0] == server {
			continue
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(server + " " + pin + "\n")

	tmp, err := os.CreateTemp(dir, ".api-pins-*") // created 0600
	if err != nil {
		return fmt.Errorf("record pin: %w", err)
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // the failure path's cleanup; after the rename there is nothing to remove
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close() //nolint:errcheck,gosec // the write error is the one to report
		return fmt.Errorf("record pin: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("record pin: %w", err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("record pin: %w", err)
	}
	return nil
}

// maxSymlinks bounds followSymlinks, as the kernel bounds a path lookup.
const maxSymlinks = 40

// followSymlinks returns the file that path names once every symlink in its
// last element is followed, which need not exist yet: a link to a file not
// yet written resolves to that file.
func followSymlinks(path string) (string, error) {
	for range maxSymlinks {
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) || err == nil && fi.Mode()&fs.ModeSymlink == 0 {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		link, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(path), link)
		}
		path = link
	}
	return "", fmt.Errorf("%s: more than %d symlinks", path, maxSymlinks)
}

// lockFile takes an exclusive flock on path, creating it (0600) if needed,
// and returns its release. The lock file is left in place: removing it would
// let a waiter lock a file a newcomer no longer sees.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: beside the pins file, as LookupPin
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd()) //nolint:gosec // G115: a file descriptor fits in an int
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		f.Close() //nolint:errcheck,gosec // the lock error is the one to report
		return nil, err
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
