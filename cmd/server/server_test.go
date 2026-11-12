package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type credentials struct {
	ID       string `json:"id"`
	Code     string `json:"code"`
	Owner    string `json:"ownerToken"`
	Receiver string `json:"receiveToken"`
}

func testApp(t *testing.T) *app {
	t.Helper()
	a, err := newApp(config{dataDir: t.TempDir(), webDir: t.TempDir(), ttl: time.Hour, maxFile: 32 << 20, maxStorage: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func request(t *testing.T, a *app, method, path, token string, body []byte, headers map[string]string, status int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, req)
	if w.Code != status {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	return w
}
func create(t *testing.T, a *app) credentials {
	t.Helper()
	var c credentials
	json.Unmarshal(request(t, a, "POST", "/api/rooms", "", nil, nil, 201).Body.Bytes(), &c)
	return c
}
func register(t *testing.T, a *app, c credentials, name string, data []byte) fileView {
	t.Helper()
	h := sha256.Sum256(data)
	b, _ := json.Marshal(map[string]any{"name": name, "size": len(data), "sha256": hex.EncodeToString(h[:])})
	var f fileView
	json.Unmarshal(request(t, a, "POST", "/api/rooms/"+c.ID+"/files", c.Owner, b, nil, 201).Body.Bytes(), &f)
	return f
}
func putChunk(t *testing.T, a *app, c credentials, f fileView, i int, data []byte, status int) {
	t.Helper()
	h := sha256.Sum256(data)
	request(t, a, "PUT", fmt.Sprintf("/api/rooms/%s/files/%s/chunks/%d", c.ID, f.ID, i), c.Owner, data, map[string]string{"X-Chunk-SHA256": hex.EncodeToString(h[:])}, status)
}
func finalize(t *testing.T, a *app, c credentials, f fileView, wantError bool) {
	t.Helper()
	request(t, a, "POST", "/api/rooms/"+c.ID+"/files/"+f.ID+"/complete", c.Owner, nil, nil, 202)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r := a.rooms[c.ID]
		r.mu.Lock()
		var ready, verifying bool
		var message string
		for _, file := range r.Files {
			if file.ID == f.ID {
				ready, verifying, message = file.Ready, file.Verifying, file.Error
			}
		}
		r.mu.Unlock()
		if !verifying {
			if wantError && message != "" {
				return
			}
			if !wantError && ready {
				return
			}
			t.Fatalf("finalize result ready=%v error=%q", ready, message)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("finalization timed out")
}
func TestTransferResumeAndRestart(t *testing.T) {
	a := testApp(t)
	c := create(t, a)
	base := "/api/rooms/" + c.ID
	if len(c.Code) != 6 || len(c.Owner) != 64 || c.Owner == c.Receiver {
		t.Fatal("invalid credentials")
	}
	request(t, a, "GET", base, "", nil, nil, 401)
	request(t, a, "POST", base+"/files", c.Receiver, []byte(`{}`), nil, 401)
	request(t, a, "POST", "/api/pair", "", []byte(`{"code":"`+c.Code+`"}`), nil, 200)
	data := bytes.Repeat([]byte("01234567"), int(chunkSize/8)+3)
	f := register(t, a, c, "../测试.txt", data)
	if f.Name != "测试.txt" {
		t.Fatal("unsafe name")
	}
	putChunk(t, a, c, f, 0, data[:chunkSize], 204)
	putChunk(t, a, c, f, 0, data[:chunkSize], 204)
	request(t, a, "POST", base+"/files/"+f.ID+"/complete", c.Owner, nil, nil, 409)
	restored, err := newApp(a.c)
	if err != nil {
		t.Fatal(err)
	}
	a = restored
	var state struct {
		Files []fileView `json:"files"`
	}
	json.Unmarshal(request(t, a, "GET", base, c.Owner, nil, nil, 200).Body.Bytes(), &state)
	if len(state.Files) != 1 || len(state.Files[0].Chunks) != 1 {
		t.Fatal("resume state not persisted")
	}
	putChunk(t, a, c, f, 1, data[chunkSize:], 204)
	finalize(t, a, c, f, false)
	request(t, a, "POST", base+"/files/"+f.ID+"/complete", c.Owner, nil, nil, 200)
	download := base + "/files/" + f.ID + "/download"
	w := request(t, a, "GET", download+"?token="+c.Receiver, "", nil, nil, 200)
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("download mismatch")
	}
	w = request(t, a, "GET", download, c.Receiver, nil, map[string]string{"Range": "bytes=2-5"}, 206)
	if w.Body.String() != string(data[2:6]) {
		t.Fatal("range mismatch")
	}
	request(t, a, "GET", download, "wrong", nil, nil, 401)
	empty := register(t, a, c, "empty.txt", nil)
	finalize(t, a, c, empty, false)
	request(t, a, "DELETE", base, c.Receiver, nil, nil, 401)
	request(t, a, "DELETE", base, c.Owner, nil, nil, 204)
	request(t, a, "GET", base, c.Owner, nil, nil, 410)
	a.cleanup()
	if _, err := os.Stat(filepath.Join(a.c.dataDir, c.ID)); !os.IsNotExist(err) {
		t.Fatal("expired data remains")
	}
}
func TestValidationAndLimits(t *testing.T) {
	a := testApp(t)
	c := create(t, a)
	base := "/api/rooms/" + c.ID
	f := register(t, a, c, "a", []byte("abc"))
	putChunk(t, a, c, f, 0, []byte("x"), 400)
	request(t, a, "PUT", base+"/files/"+f.ID+"/chunks/0", c.Owner, []byte("xyz"), map[string]string{"X-Chunk-SHA256": strings.Repeat("0", 64)}, 422)
	putChunk(t, a, c, f, 0, []byte("xyz"), 204)
	finalize(t, a, c, f, true)
	putChunk(t, a, c, f, 0, []byte("abc"), 204)
	finalize(t, a, c, f, false)
	request(t, a, "POST", base+"/files", c.Owner, []byte(`{"name":"x","size":999999999999,"sha256":"`+strings.Repeat("0", 64)+`"}`), nil, 400)
	a.c.maxStorage = 3
	request(t, a, "POST", base+"/files", c.Owner, []byte(`{"name":"x","size":1,"sha256":"`+strings.Repeat("0", 64)+`"}`), nil, 507)
	request(t, a, "POST", "/api/rooms", "", nil, map[string]string{"Origin": "https://evil.example"}, 403)
	request(t, a, "GET", "/room.json", "", nil, nil, 404)
	for i := 0; i < 20; i++ {
		request(t, a, "POST", "/api/pair", "", []byte(`{"code":"no"}`), nil, 400)
	}
	request(t, a, "POST", "/api/pair", "", []byte(`{"code":"no"}`), nil, 429)
}
func TestStreamingHTTP(t *testing.T) {
	a := testApp(t)
	c := create(t, a)
	f := register(t, a, c, "test.bin", []byte("abc"))
	server := httptest.NewServer(a.routes())
	defer server.Close()
	reader, writer := io.Pipe()
	go func() { writer.Write([]byte("ab")); writer.CloseWithError(fmt.Errorf("connection lost")) }()
	req, _ := http.NewRequest("PUT", server.URL+"/api/rooms/"+c.ID+"/files/"+f.ID+"/chunks/0", reader)
	req.Header.Set("Authorization", "Bearer "+c.Owner)
	req.Header.Set("X-Chunk-SHA256", hashString("abc"))
	res, _ := http.DefaultClient.Do(req)
	if res != nil {
		res.Body.Close()
	}
	r := a.rooms[c.ID]
	r.mu.Lock()
	view := a.fileView(r, r.Files[0])
	r.mu.Unlock()
	if len(view.Chunks) != 0 {
		t.Fatal("incomplete chunk advertised")
	}
}

func TestStartupRecovery(t *testing.T) {
	a := testApp(t)
	orphan := filepath.Join(a.c.dataDir, randomHex(16))
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, ".metadata-partial"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	c := create(t, a)
	f := register(t, a, c, "test.txt", []byte("abc"))
	putChunk(t, a, c, f, 0, []byte("abc"), 204)
	finalize(t, a, c, f, false)
	r := a.rooms[c.ID]
	if err := os.WriteFile(a.chunkPath(r, r.Files[0], 0), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := newApp(a.c)
	if err != nil {
		t.Fatal(err)
	}
	if restored.reserved != 3 {
		t.Fatal("quota was not restored")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("abandoned room remains")
	}
	if _, err := os.Stat(a.chunkPath(r, r.Files[0], 0)); !os.IsNotExist(err) {
		t.Fatal("redundant chunk remains")
	}
}
