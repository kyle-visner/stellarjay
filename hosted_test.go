package stellarjay

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendAtSerializesConcurrentWriters(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	const writers = 24
	start := make(chan struct{})
	results := make(chan error, writers)
	var group sync.WaitGroup
	for i := 0; i < writers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := store.AppendAt(Context{Actor: "writer", Role: "writer"}, AppendOptions{
				Type: "fact", Command: "fact create", Payload: map[string]bool{"ok": true},
			}, "")
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)

	succeeded, conflicted := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		var app *AppError
		if errors.As(err, &app) && app.Code == ErrConflict {
			conflicted++
			continue
		}
		t.Fatalf("unexpected append error: %v", err)
	}
	if succeeded != 1 || conflicted != writers-1 {
		t.Fatalf("expected one success and %d conflicts, got %d and %d", writers-1, succeeded, conflicted)
	}
}

func TestAppendIdempotentSurvivesNewerEvents(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, replayed, err := store.AppendIdempotent(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]string{"value": "one"},
	}, "", "request-one", "content-one")
	if err != nil || replayed {
		t.Fatalf("first append: replayed=%v err=%v", replayed, err)
	}
	second, err := store.AppendAt(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]string{"value": "two"},
	}, first)
	if err != nil {
		t.Fatal(err)
	}
	retried, replayed, err := store.AppendIdempotent(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]string{"value": "one"},
	}, "", "request-one", "content-one")
	if err != nil || !replayed || retried != first {
		t.Fatalf("retry: hash=%q replayed=%v err=%v", retried, replayed, err)
	}
	root, err := store.CurrentRoot()
	if err != nil || root != second {
		t.Fatalf("retry changed current root: root=%q err=%v", root, err)
	}
}

func TestSnapshotExcludesKeysAndPlaintext(t *testing.T) {
	storeDir := t.TempDir()
	store, err := OpenStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	secret := "snapshot-must-not-contain-this-plaintext"
	if _, err := store.Append(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]string{"secret": secret},
	}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "snapshot.tar.gz")
	if _, err := store.Snapshot(dest); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	seenManifest, seenRoot, seenNode := false, false, false
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(header.Name, "keys/") {
			t.Fatalf("snapshot included key path %q", header.Name)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), secret) {
			t.Fatalf("snapshot entry %q contains plaintext payload", header.Name)
		}
		switch {
		case header.Name == "manifest.json":
			seenManifest = true
		case header.Name == "refs/root":
			seenRoot = true
		case strings.HasPrefix(header.Name, "objects/nodes/"):
			seenNode = true
		}
	}
	if !seenManifest || !seenRoot || !seenNode {
		t.Fatalf("incomplete snapshot: manifest=%v root=%v node=%v", seenManifest, seenRoot, seenNode)
	}

	keyBytes, err := os.ReadFile(filepath.Join(storeDir, "keys", "data.key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(keyBytes))); err != nil {
		t.Fatalf("local test key should remain separately recoverable: %v", err)
	}
}

func TestNodePayloadRejectsBadNonceWithoutPanicking(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	node := Node{Hash: "sha256:" + strings.Repeat("0", 64), SealedPayload: &EncryptedPayload{
		Algorithm: "AES-256-GCM", Nonce: base64.StdEncoding.EncodeToString([]byte("short")),
		Ciphertext: base64.StdEncoding.EncodeToString([]byte("ciphertext")),
	}}
	if _, err := store.NodePayload(node); err == nil {
		t.Fatal("expected invalid nonce to be rejected")
	}
}

func TestVerifyHeadRejectsMisaddressedNode(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Append(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]int{"value": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Append(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]int{"value": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(store.NodePath(first))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.NodePath(second), firstBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyHead(); err == nil {
		t.Fatal("expected a node stored under the wrong address to fail readiness")
	}
}

func TestBDDEventPageReadsOnlyTheRequestedWindow(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var roots []string
	for i := 0; i < 4; i++ {
		root, err := store.Append(Context{Actor: "agent"}, AppendOptions{
			Type: "fact", Command: "remember", Payload: map[string]int{"sequence": i},
		})
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, root)
	}

	// Corrupt a node outside the first requested page. A bounded page must not
	// materialize or verify the entire history as a side effect.
	thirdPath := store.NodePath(roots[2])
	thirdBytes, err := os.ReadFile(thirdPath)
	if err != nil {
		t.Fatal(err)
	}
	thirdBytes = []byte(strings.Replace(string(thirdBytes), "ciphertext", "ciphertexu", 1))
	if err := os.WriteFile(thirdPath, thirdBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	page, err := store.EventPage("", 1)
	if err != nil {
		t.Fatalf("first bounded page read unrelated corrupt node: %v", err)
	}
	if len(page.Nodes) != 1 || page.Nodes[0].Hash != roots[0] || !page.HasMore || page.Root != roots[3] {
		t.Fatalf("unexpected first page: %#v", page)
	}
	if _, err := store.EventPage(roots[1], 2); err == nil {
		t.Fatal("compatibility event page stopped verifying selected node integrity")
	}
	page, err = store.MetadataEventPageAt(roots[1], roots[3], 2)
	if err != nil || len(page.Nodes) != 2 || page.Nodes[0].Hash != roots[2] {
		t.Fatalf("metadata replay was blocked by corrupt payload: page=%#v err=%v", page, err)
	}
	if _, err := store.EventPayloads([]string{roots[2]}, roots[3]); err == nil {
		t.Fatal("expected selective retrieval of the corrupt payload to fail integrity verification")
	}
}

func TestMetadataReplaySkipsKeyMismatchedPayloadAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for i, nodeType := range []string{"magpie.fact", "foreign.fact", "martin.fact"} {
		root, appendErr := store.Append(Context{Actor: "fixture", Role: "writer"}, AppendOptions{
			Type: nodeType, Command: "remember", Payload: map[string]int{"sequence": i},
		})
		if appendErr != nil {
			t.Fatal(appendErr)
		}
		roots = append(roots, root)
	}
	nodes, err := store.NodesFromRoot(roots[len(roots)-1])
	if err != nil {
		t.Fatal(err)
	}
	wrongKey := append([]byte(nil), store.key...)
	wrongKey[0] ^= 0xff
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	nodes[1].SealedPayload, err = encryptPayload(wrongKey, []byte(`{"sequence":1}`))
	if err != nil {
		t.Fatal(err)
	}
	rehash := func(node *Node) {
		content := nodeContent{
			Schema: node.Schema, Type: node.Type, EntityID: node.EntityID, Parents: node.Parents,
			Payload: node.Payload, SealedPayload: node.SealedPayload, Actor: node.Actor, Role: node.Role,
			Command: node.Command, CreatedAt: node.CreatedAt, RequestID: node.RequestID, RequestHash: node.RequestHash,
		}
		encoded, marshalErr := json.Marshal(content)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		sum := sha256.Sum256(encoded)
		node.Hash = "sha256:" + hex.EncodeToString(sum[:])
	}
	rehash(&nodes[1])
	nodes[2].Parents = []string{nodes[1].Hash}
	rehash(&nodes[2])
	for _, node := range nodes[1:] {
		encoded, marshalErr := json.MarshalIndent(node, "", "  ")
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		path := filepath.Join(dir, "objects", "nodes", strings.TrimPrefix(node.Hash, "sha256:")+".json")
		if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "refs", "root"), []byte(nodes[2].Hash+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen with a key-mismatched non-head payload: %v", err)
	}
	defer store.Close()
	page, err := store.MetadataEventPageAt("", nodes[2].Hash, 10)
	if err != nil || len(page.Nodes) != 3 || page.Nodes[1].Hash != nodes[1].Hash {
		t.Fatalf("metadata replay was blocked by key mismatch: page=%#v err=%v", page, err)
	}
	payloads, err := store.EventPayloads([]string{nodes[0].Hash, nodes[2].Hash}, nodes[2].Hash)
	if err != nil || len(payloads) != 2 {
		t.Fatalf("valid selected payloads were blocked by unselected key mismatch: payloads=%#v err=%v", payloads, err)
	}
	if _, err := store.EventPayloads([]string{nodes[1].Hash}, nodes[2].Hash); err == nil {
		t.Fatal("selected key-mismatched payload unexpectedly decrypted")
	}
}

func TestEventPayloadSelectionReleasesStoreLockBeforeNodeRead(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root, err := store.Append(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]int{"sequence": 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		_, readErr := store.eventPayloadsWithReader([]string{root}, root, 1024, func(hash string) (Node, error) {
			close(readStarted)
			<-releaseRead
			return store.readNode(hash)
		})
		readDone <- readErr
	}()
	<-readStarted

	appendDone := make(chan error, 1)
	go func() {
		_, appendErr := store.Append(Context{Actor: "writer"}, AppendOptions{
			Type: "fact", Command: "remember", Payload: map[string]int{"sequence": 2},
		})
		appendDone <- appendErr
	}()
	select {
	case err := <-appendDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(releaseRead)
		<-readDone
		t.Fatal("payload node read held the store lock and blocked an append")
	}
	close(releaseRead)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
}

func TestBDDIncrementalReplayStopsAtCapturedRootWhenLiveRootAdvances(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var roots []string
	appendEvent := func(sequence int) {
		t.Helper()
		root, err := store.Append(Context{Actor: "writer"}, AppendOptions{
			Type: "fact", Command: "remember", Payload: map[string]int{"sequence": sequence},
		})
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, root)
	}
	for i := 0; i < 4; i++ {
		appendEvent(i)
	}

	first, err := store.EventPage("", 3)
	if err != nil {
		t.Fatal(err)
	}
	target := first.Root
	if target != roots[3] || len(first.Nodes) != 3 || !first.HasMore {
		t.Fatalf("unexpected first page: %#v", first)
	}

	// Model another writer advancing the live root between page requests.
	appendEvent(4)
	appendEvent(5)
	second, err := store.EventPage(first.Nodes[len(first.Nodes)-1].Hash, 3)
	if err != nil {
		t.Fatal(err)
	}
	if second.Root != roots[5] || len(second.Nodes) != 3 || second.HasMore {
		t.Fatalf("unexpected second page after concurrent appends: %#v", second)
	}

	applied := make([]string, 0, 4)
	for _, node := range first.Nodes {
		applied = append(applied, node.Hash)
	}
	reachedTarget := false
	for _, node := range second.Nodes {
		applied = append(applied, node.Hash)
		if node.Hash == target {
			reachedTarget = true
			break
		}
	}
	if !reachedTarget {
		t.Fatalf("captured target %s was not reachable in the advanced history", target)
	}
	if len(applied) != 4 {
		t.Fatalf("applied events beyond captured target: %#v", applied)
	}
	for i, hash := range applied {
		if hash != roots[i] {
			t.Fatalf("applied event %d = %s, want %s", i, hash, roots[i])
		}
	}

	caughtUp, err := store.EventPage(roots[5], 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(caughtUp.Nodes) != 0 || caughtUp.Root != roots[5] || caughtUp.HasMore {
		t.Fatalf("current-root checkpoint did not terminate with an empty page: %#v", caughtUp)
	}
}

func TestBDDRequestIndexRebuildsAndAvoidsHistoryScanOnReplay(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, replayed, err := store.AppendIdempotent(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]int{"sequence": 1},
	}, "", "request-one", "content-one")
	if err != nil || replayed {
		t.Fatalf("first append: replayed=%v err=%v", replayed, err)
	}
	second, err := store.AppendAt(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]int{"sequence": 2},
	}, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendAt(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]int{"sequence": 3},
	}, second); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if record, ok := store.requestIndex["request-one"]; !ok || record.Hash != first || record.RequestHash != "content-one" {
		t.Fatalf("request index was not rebuilt: %#v, %v", record, ok)
	}

	// Removing an unrelated historical node makes a linear backward scan fail.
	// Indexed replay should still return the already committed request directly.
	if err := os.Remove(store.NodePath(second)); err != nil {
		t.Fatal(err)
	}
	hash, replayed, err := store.AppendIdempotent(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]int{"sequence": 1},
	}, "", "request-one", "content-one")
	if err != nil || !replayed || hash != first {
		t.Fatalf("indexed replay: hash=%q replayed=%v err=%v", hash, replayed, err)
	}
}

func TestBDDOpeningWithWrongDataKeyFailsBeforeServing(t *testing.T) {
	dir := t.TempDir()
	firstKey := strings.Repeat("11", 32)
	wrongKey := strings.Repeat("22", 32)
	store, err := OpenStoreWithDataKey(dir, firstKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(Context{Actor: "agent"}, AppendOptions{
		Type: "fact", Command: "remember", Payload: map[string]string{"secret": "correct key"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = OpenStoreWithDataKey(dir, wrongKey)
	var app *AppError
	if !errors.As(err, &app) || app.Code != ErrIntegrity {
		t.Fatalf("expected wrong key to fail with integrity error, got %v", err)
	}
}

func TestBDDStoreLockRejectsASecondWriter(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenStore(dir)
	if second != nil {
		second.Close()
	}
	var app *AppError
	if !errors.As(err, &app) || app.Code != ErrConflict {
		t.Fatalf("expected second store owner to be rejected, got %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("lock was not released on close: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
