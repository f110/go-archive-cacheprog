package cacheprog

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type session struct {
	t     *testing.T
	enc   *json.Encoder
	dec   *json.Decoder
	in    *io.PipeWriter
	out   *io.PipeReader
	err   <-chan error
	stats *bytes.Buffer
}

func newSession(t *testing.T, archivePath string) *session {
	t.Helper()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	stats := &bytes.Buffer{}
	errCh := make(chan error, 1)
	go func() {
		err := Run(context.Background(), archivePath, inR, outW, stats)
		outW.Close()
		errCh <- err
	}()

	s := &session{
		t:     t,
		enc:   json.NewEncoder(inW),
		dec:   json.NewDecoder(outR),
		in:    inW,
		out:   outR,
		err:   errCh,
		stats: stats,
	}

	var handshake Response
	if err := s.dec.Decode(&handshake); err != nil {
		t.Fatalf("decode handshake: %v", err)
	}
	if len(handshake.KnownCommands) == 0 {
		t.Fatalf("expected handshake to advertise commands, got %+v", handshake)
	}
	return s
}

func (s *session) send(v any) {
	s.t.Helper()
	if err := s.enc.Encode(v); err != nil {
		s.t.Fatalf("encode: %v", err)
	}
}

func (s *session) recv() Response {
	s.t.Helper()
	var r Response
	if err := s.dec.Decode(&r); err != nil {
		s.t.Fatalf("decode response: %v", err)
	}
	return r
}

func (s *session) closeAndWait() Response {
	s.t.Helper()
	s.send(Request{ID: 99, Command: CmdClose})
	resp := s.recv()
	s.in.Close()
	if err := <-s.err; err != nil {
		s.t.Fatalf("Run returned error: %v", err)
	}
	return resp
}

func TestServe_GetMissPutGetHit(t *testing.T) {
	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "cache.zip")

	actionID := []byte{0x01, 0x02, 0x03}
	outputID := []byte{0xaa, 0xbb}
	payload := []byte("hello, archive cache")

	s := newSession(t, archivePath)

	s.send(Request{ID: 1, Command: CmdGet, ActionID: actionID})
	if r := s.recv(); !r.Miss || r.ID != 1 {
		t.Fatalf("expected miss, got %+v", r)
	}

	s.send(Request{ID: 2, Command: CmdPut, ActionID: actionID, OutputID: outputID, BodySize: int64(len(payload))})
	s.send(payload)
	putResp := s.recv()
	if putResp.ID != 2 || putResp.Err != "" || putResp.DiskPath == "" {
		t.Fatalf("unexpected put response: %+v", putResp)
	}
	if got, err := os.ReadFile(putResp.DiskPath); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("put disk content mismatch (err=%v): got %q, want %q", err, got, payload)
	}

	s.send(Request{ID: 3, Command: CmdGet, ActionID: actionID})
	hit := s.recv()
	if hit.ID != 3 || hit.Miss {
		t.Fatalf("expected hit, got %+v", hit)
	}
	if !bytes.Equal(hit.OutputID, outputID) {
		t.Fatalf("output id mismatch: got %x, want %x", hit.OutputID, outputID)
	}
	if hit.Size != int64(len(payload)) {
		t.Fatalf("size mismatch: got %d, want %d", hit.Size, len(payload))
	}
	if got, err := os.ReadFile(hit.DiskPath); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("hit disk content mismatch (err=%v): got %q, want %q", err, got, payload)
	}

	if r := s.closeAndWait(); r.Err != "" {
		t.Fatalf("close returned error: %s", r.Err)
	}

	assertArchiveContains(t, archivePath, actionID, outputID, payload)
}

func TestServe_PersistAcrossSessions(t *testing.T) {
	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "cache.zip")

	actionID := []byte{0x10, 0x20}
	outputID := []byte{0xcc}
	payload := []byte("persisted across sessions")

	s := newSession(t, archivePath)
	s.send(Request{ID: 1, Command: CmdPut, ActionID: actionID, OutputID: outputID, BodySize: int64(len(payload))})
	s.send(payload)
	if r := s.recv(); r.Err != "" {
		t.Fatalf("put error: %s", r.Err)
	}
	s.closeAndWait()

	s2 := newSession(t, archivePath)
	s2.send(Request{ID: 1, Command: CmdGet, ActionID: actionID})
	hit := s2.recv()
	if hit.Miss {
		t.Fatalf("expected hit from previous session, got miss")
	}
	if !bytes.Equal(hit.OutputID, outputID) {
		t.Fatalf("output id mismatch: got %x, want %x", hit.OutputID, outputID)
	}
	got, err := os.ReadFile(hit.DiskPath)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch (err=%v): got %q, want %q", err, got, payload)
	}
	s2.closeAndWait()
}

func TestServe_OverwriteExistingActionID(t *testing.T) {
	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "cache.zip")

	actionID := []byte{0xab, 0xcd}
	first := []byte("first payload")
	second := []byte("second payload, longer than first")

	s := newSession(t, archivePath)
	s.send(Request{ID: 1, Command: CmdPut, ActionID: actionID, OutputID: []byte{0x01}, BodySize: int64(len(first))})
	s.send(first)
	s.recv()
	s.closeAndWait()

	s2 := newSession(t, archivePath)
	s2.send(Request{ID: 1, Command: CmdPut, ActionID: actionID, OutputID: []byte{0x02}, BodySize: int64(len(second))})
	s2.send(second)
	s2.recv()
	s2.closeAndWait()

	assertArchiveContains(t, archivePath, actionID, []byte{0x02}, second)

	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer zr.Close()
	if len(zr.File) != 1 {
		t.Fatalf("expected exactly 1 entry (overwrite), got %d", len(zr.File))
	}
}

func TestServe_ConcurrentGets(t *testing.T) {
	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "cache.zip")

	type entry struct {
		actionID []byte
		outputID []byte
		payload  []byte
	}
	const n = 16
	entries := make([]entry, n)
	for i := range entries {
		entries[i] = entry{
			actionID: []byte{byte(i + 1), 0xff, byte(i ^ 0x5a)},
			outputID: []byte{byte(i + 1), byte(i + 100)},
			payload:  []byte(fmt.Sprintf("payload-%d-%s", i, bytes.Repeat([]byte("x"), i*32))),
		}
	}

	populate := newSession(t, archivePath)
	for i, e := range entries {
		populate.send(Request{ID: int64(i + 1), Command: CmdPut, ActionID: e.actionID, OutputID: e.outputID, BodySize: int64(len(e.payload))})
		populate.send(e.payload)
		if r := populate.recv(); r.Err != "" {
			t.Fatalf("put %d failed: %s", i, r.Err)
		}
	}
	populate.closeAndWait()

	s := newSession(t, archivePath)
	for i, e := range entries {
		s.send(Request{ID: int64(i + 1), Command: CmdGet, ActionID: e.actionID})
	}

	received := make(map[int64]Response, n)
	for range entries {
		r := s.recv()
		if _, dup := received[r.ID]; dup {
			t.Fatalf("duplicate response for ID %d", r.ID)
		}
		received[r.ID] = r
	}

	for i, e := range entries {
		r, ok := received[int64(i + 1)]
		if !ok {
			t.Fatalf("missing response for ID %d", i+1)
		}
		if r.Miss || r.Err != "" {
			t.Fatalf("ID %d unexpected response: %+v", i+1, r)
		}
		if !bytes.Equal(r.OutputID, e.outputID) {
			t.Fatalf("ID %d output id mismatch: got %x, want %x", i+1, r.OutputID, e.outputID)
		}
		got, err := os.ReadFile(r.DiskPath)
		if err != nil {
			t.Fatalf("ID %d read %s: %v", i+1, r.DiskPath, err)
		}
		if !bytes.Equal(got, e.payload) {
			t.Fatalf("ID %d content mismatch", i+1)
		}
	}

	s.closeAndWait()
}

func TestServe_ConcurrentGetsSameActionID(t *testing.T) {
	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "cache.zip")

	actionID := []byte{0x77, 0x88}
	outputID := []byte{0x99}
	payload := bytes.Repeat([]byte("dedup-extract-payload"), 1024)

	populate := newSession(t, archivePath)
	populate.send(Request{ID: 1, Command: CmdPut, ActionID: actionID, OutputID: outputID, BodySize: int64(len(payload))})
	populate.send(payload)
	populate.recv()
	populate.closeAndWait()

	s := newSession(t, archivePath)
	const n = 8
	for i := 0; i < n; i++ {
		s.send(Request{ID: int64(i + 1), Command: CmdGet, ActionID: actionID})
	}

	var path string
	for i := 0; i < n; i++ {
		r := s.recv()
		if r.Miss || r.Err != "" {
			t.Fatalf("response %d unexpected: %+v", i, r)
		}
		if path == "" {
			path = r.DiskPath
		} else if r.DiskPath != path {
			t.Fatalf("DiskPath mismatch across concurrent gets: %s vs %s", path, r.DiskPath)
		}
		got, err := os.ReadFile(r.DiskPath)
		if err != nil {
			t.Fatalf("read %s: %v", r.DiskPath, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("content mismatch on response %d", i)
		}
	}
	s.closeAndWait()
}

func TestServe_StatsOutput(t *testing.T) {
	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "cache.zip")

	actionA := []byte{0xa1}
	outputA := []byte{0xa2}
	payloadA := []byte("aaaaaaaaaa")

	populate := newSession(t, archivePath)
	populate.send(Request{ID: 1, Command: CmdPut, ActionID: actionA, OutputID: outputA, BodySize: int64(len(payloadA))})
	populate.send(payloadA)
	populate.recv()
	populate.closeAndWait()

	populateStats := populate.stats.String()
	for _, want := range []string{
		"puts:          1",
		"entries:       1 total (new: 1, replaced: 0)",
		"archive size:",
		"update time:",
	} {
		if !strings.Contains(populateStats, want) {
			t.Fatalf("populate stats missing %q:\n%s", want, populateStats)
		}
	}

	s := newSession(t, archivePath)
	s.send(Request{ID: 1, Command: CmdGet, ActionID: actionA})
	s.recv()
	s.send(Request{ID: 2, Command: CmdGet, ActionID: []byte{0xff}})
	s.recv()
	s.closeAndWait()

	stats := s.stats.String()
	for _, want := range []string{
		"gets:          2 (hits: 1, misses: 1)",
		"hit rate:      50.0%",
		"hit bytes:     10 B",
		"puts:          0",
	} {
		if !strings.Contains(stats, want) {
			t.Fatalf("stats output missing %q:\n%s", want, stats)
		}
	}
	if strings.Contains(stats, "archive size:") {
		t.Fatalf("read-only session must not report archive size:\n%s", stats)
	}
	if strings.Contains(stats, "entries:") {
		t.Fatalf("read-only session must not report entries:\n%s", stats)
	}
	if strings.Contains(stats, "update time:") {
		t.Fatalf("read-only session must not report update time:\n%s", stats)
	}
}

func TestServe_FlushStatsReplacement(t *testing.T) {
	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "cache.zip")

	actionA := []byte{0xa1}
	actionB := []byte{0xb2}
	payload := bytes.Repeat([]byte("x"), 256)

	s1 := newSession(t, archivePath)
	for i, a := range [][]byte{actionA, actionB} {
		s1.send(Request{ID: int64(i + 1), Command: CmdPut, ActionID: a, OutputID: []byte{0x01}, BodySize: int64(len(payload))})
		s1.send(payload)
		s1.recv()
	}
	s1.closeAndWait()

	s2 := newSession(t, archivePath)
	s2.send(Request{ID: 1, Command: CmdPut, ActionID: actionA, OutputID: []byte{0x02}, BodySize: int64(len(payload))})
	s2.send(payload)
	s2.recv()
	s2.closeAndWait()

	stats := s2.stats.String()
	if !strings.Contains(stats, "entries:       2 total (new: 1, replaced: 1)") {
		t.Fatalf("expected entries line with replaced=1:\n%s", stats)
	}
}

func TestServe_MissingArchiveCreatesEmptyOnClose(t *testing.T) {
	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "subdir", "cache.zip")

	s := newSession(t, archivePath)

	s.send(Request{ID: 1, Command: CmdGet, ActionID: []byte{0x01, 0x02}})
	if r := s.recv(); !r.Miss || r.Err != "" {
		t.Fatalf("expected miss on missing archive, got %+v", r)
	}

	s.closeAndWait()

	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("expected archive to be created on close, got %v", err)
	}
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatalf("created archive is not a valid zip: %v", err)
	}
	defer zr.Close()
	if len(zr.File) != 0 {
		t.Fatalf("expected empty zip, got %d entries", len(zr.File))
	}
}

func assertArchiveContains(t *testing.T, archivePath string, actionID, outputID, payload []byte) {
	t.Helper()
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer zr.Close()

	wantName := hex.EncodeToString(actionID) + "-" + hex.EncodeToString(outputID)
	for _, f := range zr.File {
		if f.Name != wantName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open entry: %v", err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read entry: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("archive content mismatch: got %q, want %q", got, payload)
		}
		return
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	t.Fatalf("entry %q not found in archive (have: %v)", wantName, names)
}
