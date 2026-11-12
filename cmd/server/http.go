package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	reply(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		fail(w, 400, "请求数据无效")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fail(w, 400, "请求数据无效")
		return false
	}
	return true
}
func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"chunkSize": chunkSize, "maxFileBytes": a.c.maxFile, "maxFiles": maxFiles, "roomTTLHours": int(a.c.ttl / time.Hour)})
	})
	mux.HandleFunc("POST /api/rooms", a.createRoom)
	mux.HandleFunc("POST /api/pair", a.pair)
	mux.HandleFunc("GET /api/rooms/{room}", a.roomStatus)
	mux.HandleFunc("DELETE /api/rooms/{room}", a.deleteRoom)
	mux.HandleFunc("POST /api/rooms/{room}/files", a.addFile)
	mux.HandleFunc("PUT /api/rooms/{room}/files/{file}/chunks/{index}", a.uploadChunk)
	mux.HandleFunc("POST /api/rooms/{room}/files/{file}/complete", a.completeFile)
	mux.HandleFunc("GET /api/rooms/{room}/files/{file}/download", a.download)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "接口不存在") })
	static := http.FileServer(http.Dir(a.c.webDir))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/assets/") {
			http.NotFound(w, r)
			return
		}
		static.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' data:; worker-src 'self' blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if r.Method != "GET" && r.Method != "HEAD" {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host || (u.Scheme != "https" && u.Scheme != "http") {
					fail(w, 403, "不允许跨站请求")
					return
				}
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				fail(w, 403, "不允许跨站请求")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
func (a *app) limited(w http.ResponseWriter, r *http.Request, kind string, limit int) bool {
	if !a.allow(kind+clientIP(r), limit) {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "请求过于频繁，请稍后再试")
		return true
	}
	return false
}
func (a *app) createRoom(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "create", 10) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.rooms) >= maxRooms {
		fail(w, 503, "房间数量已达上限，请等待过期清理")
		return
	}
	code := ""
	for code == "" {
		n, err := rand.Int(rand.Reader, big.NewInt(1000000))
		if err != nil {
			fail(w, 500, "随机数生成失败")
			return
		}
		code = fmt.Sprintf("%06d", n.Int64())
		for _, room := range a.rooms {
			if room.Code == code {
				code = ""
				break
			}
		}
	}
	owner := randomHex(32)
	room := &room{ID: randomHex(16), Code: code, OwnerHash: hashString(owner), ReceiveToken: randomHex(32), Expires: time.Now().UTC().Add(a.c.ttl), Files: []*FileRecord{}}
	if err := os.Mkdir(a.roomDir(room), 0700); err != nil {
		fail(w, 500, "无法创建存储目录")
		return
	}
	if err := a.save(room); err != nil {
		os.Remove(a.roomDir(room))
		fail(w, 500, "无法保存房间")
		return
	}
	a.rooms[room.ID] = room
	reply(w, 201, map[string]any{"id": room.ID, "code": code, "ownerToken": owner, "receiveToken": room.ReceiveToken, "expires": room.Expires})
}
func (a *app) pair(w http.ResponseWriter, r *http.Request) {
	if a.limited(w, r, "pair", 20) {
		return
	}
	var input struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.Code) != 6 || strings.Trim(input.Code, "0123456789") != "" {
		fail(w, 400, "请输入六位数字配对码")
		return
	}
	a.mu.Lock()
	var found *room
	for _, room := range a.rooms {
		if room.Code == input.Code {
			found = room
			break
		}
	}
	a.mu.Unlock()
	if found == nil {
		fail(w, 404, "配对码无效或房间已过期")
		return
	}
	found.mu.Lock()
	defer found.mu.Unlock()
	if time.Now().After(found.Expires) {
		fail(w, 404, "配对码无效或房间已过期")
		return
	}
	reply(w, 200, map[string]any{"id": found.ID, "receiveToken": found.ReceiveToken, "expires": found.Expires})
}

// The returned room is locked. Each caller must unlock it.
func (a *app) authorize(w http.ResponseWriter, r *http.Request, ownerOnly bool) *room {
	a.mu.Lock()
	room := a.rooms[r.PathValue("room")]
	a.mu.Unlock()
	if room == nil {
		fail(w, 404, "房间不存在或已过期")
		return nil
	}
	room.mu.Lock()
	if time.Now().After(room.Expires) {
		room.mu.Unlock()
		fail(w, 410, "房间已过期")
		return nil
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.Method == "GET" && r.PathValue("file") != "" && token == "" {
		token = r.URL.Query().Get("token")
	}
	owner := subtle.ConstantTimeCompare([]byte(hashString(token)), []byte(room.OwnerHash)) == 1
	receiver := subtle.ConstantTimeCompare([]byte(token), []byte(room.ReceiveToken)) == 1
	if !owner && (ownerOnly || !receiver) {
		room.mu.Unlock()
		fail(w, 401, "访问凭证无效，请重新配对")
		return nil
	}
	return room
}

type fileView struct {
	*FileRecord
	Chunks    []int `json:"chunks"`
	Verifying bool  `json:"verifying"`
}

func (a *app) fileView(room *room, file *FileRecord) fileView {
	v := fileView{FileRecord: file, Chunks: []int{}, Verifying: file.Verifying}
	if !file.Ready {
		for i := 0; i < chunkCount(file); i++ {
			if info, err := os.Stat(a.chunkPath(room, file, i)); err == nil && info.Size() == expectedSize(file, i) {
				v.Chunks = append(v.Chunks, i)
			}
		}
	}
	return v
}
func (a *app) roomStatus(w http.ResponseWriter, r *http.Request) {
	room := a.authorize(w, r, false)
	if room == nil {
		return
	}
	defer room.mu.Unlock()
	files := make([]fileView, 0, len(room.Files))
	for _, f := range room.Files {
		files = append(files, a.fileView(room, f))
	}
	reply(w, 200, map[string]any{"id": room.ID, "expires": room.Expires, "files": files})
}
func (a *app) deleteRoom(w http.ResponseWriter, r *http.Request) {
	room := a.authorize(w, r, true)
	if room == nil {
		return
	}
	defer room.mu.Unlock()
	// Persist revocation before deleting data, so a restart cannot restore access.
	room.Expires = time.Now().UTC()
	if err := a.save(room); err != nil {
		fail(w, 500, "无法关闭房间")
		return
	}
	w.WriteHeader(204)
}
func safeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune("<>:\"|?*", r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" || name == "." {
		name = "file"
	}
	if len([]rune(name)) > 160 {
		name = string([]rune(name)[:160])
	}
	return name
}
func (a *app) addFile(w http.ResponseWriter, r *http.Request) {
	room := a.authorize(w, r, true)
	if room == nil {
		return
	}
	defer room.mu.Unlock()
	var input struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if !decode(w, r, &input) {
		return
	}
	hash, err := hex.DecodeString(input.SHA256)
	if input.Size < 0 || input.Size > a.c.maxFile || len(hash) != 32 || err != nil || input.Name == "" {
		fail(w, 400, "文件名、大小或 SHA-256 无效")
		return
	}
	name := safeName(input.Name)
	input.SHA256 = strings.ToLower(input.SHA256)
	for _, f := range room.Files {
		if f.Name == name && f.Size == input.Size && f.SHA256 == input.SHA256 {
			reply(w, 200, a.fileView(room, f))
			return
		}
	}
	if len(room.Files) >= maxFiles {
		fail(w, 400, "每个房间最多 100 个文件")
		return
	}
	a.mu.Lock()
	if input.Size > a.c.maxStorage-a.reserved {
		a.mu.Unlock()
		fail(w, 507, "存储配额不足")
		return
	}
	a.reserved += input.Size
	a.mu.Unlock()
	file := &FileRecord{ID: randomHex(16), Name: name, Size: input.Size, SHA256: input.SHA256}
	room.Files = append(room.Files, file)
	err = os.Mkdir(a.fileDir(room, file), 0700)
	if err == nil {
		err = a.save(room)
	}
	if err != nil {
		room.Files = room.Files[:len(room.Files)-1]
		a.mu.Lock()
		a.reserved -= input.Size
		a.mu.Unlock()
		os.Remove(a.fileDir(room, file))
		fail(w, 500, "无法保存文件信息")
		return
	}
	reply(w, 201, a.fileView(room, file))
}
func findFile(room *room, id string) *FileRecord {
	for _, f := range room.Files {
		if f.ID == id {
			return f
		}
	}
	return nil
}
func (a *app) uploadChunk(w http.ResponseWriter, r *http.Request) {
	room := a.authorize(w, r, true)
	if room == nil {
		return
	}
	defer room.mu.Unlock()
	f := findFile(room, r.PathValue("file"))
	if f == nil {
		fail(w, 404, "文件不存在")
		return
	}
	i, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || i < 0 || i >= chunkCount(f) {
		fail(w, 400, "分块编号无效")
		return
	}
	if f.Ready || f.Verifying {
		fail(w, 409, "文件已完成")
		return
	}
	want := strings.ToLower(r.Header.Get("X-Chunk-SHA256"))
	sum, err := hex.DecodeString(want)
	if err != nil || len(sum) != 32 {
		fail(w, 400, "缺少分块 SHA-256")
		return
	}
	length := expectedSize(f, i)
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(3 * time.Minute))
	r.Body = http.MaxBytesReader(w, r.Body, length+1)
	temp, err := os.CreateTemp(a.fileDir(room, f), ".chunk-*")
	if err != nil {
		fail(w, 507, "磁盘无法写入")
		return
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(temp, h), r.Body)
	if err != nil || n != length {
		fail(w, 400, "分块大小不匹配或上传中断")
		return
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		fail(w, 422, "分块校验失败，请重试")
		return
	}
	if err = temp.Sync(); err == nil {
		err = temp.Close()
	}
	if err == nil {
		err = os.Rename(temp.Name(), a.chunkPath(room, f, i))
	}
	if err != nil {
		fail(w, 507, "分块保存失败")
		return
	}
	w.WriteHeader(204)
}
func (a *app) completeFile(w http.ResponseWriter, r *http.Request) {
	room := a.authorize(w, r, true)
	if room == nil {
		return
	}
	defer room.mu.Unlock()
	f := findFile(room, r.PathValue("file"))
	if f == nil {
		fail(w, 404, "文件不存在")
		return
	}
	if f.Ready {
		reply(w, 200, f)
		return
	}
	if f.Verifying {
		reply(w, 202, map[string]bool{"verifying": true})
		return
	}
	if len(a.fileView(room, f).Chunks) != chunkCount(f) {
		fail(w, 409, "分块尚未上传完整")
		return
	}
	select {
	case a.verificationSlots <- struct{}{}:
	default:
		fail(w, 503, "服务器正在校验其他文件，请稍后继续上传")
		return
	}
	f.Verifying = true
	f.Error = ""
	go func() {
		defer func() { <-a.verificationSlots }()
		err := a.assemble(room, f)
		room.mu.Lock()
		defer room.mu.Unlock()
		f.Verifying = false
		if err != nil {
			f.Error = err.Error()
			return
		}
		f.Ready = true
		if err = a.save(room); err != nil {
			f.Ready = false
			f.Error = "文件状态保存失败，请重试"
			return
		}
		for i := 0; i < chunkCount(f); i++ {
			os.Remove(a.chunkPath(room, f, i))
		}
	}()
	reply(w, 202, map[string]bool{"verifying": true})
}

func (a *app) assemble(room *room, f *FileRecord) error {
	temp, err := os.CreateTemp(a.fileDir(room, f), ".complete-*")
	if err != nil {
		return errors.New("磁盘无法写入")
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	h := sha256.New()
	for i := 0; i < chunkCount(f); i++ {
		part, openErr := os.Open(a.chunkPath(room, f, i))
		if openErr != nil {
			return errors.New("分块尚未上传完整")
		}
		n, copyErr := io.Copy(io.MultiWriter(temp, h), part)
		part.Close()
		if copyErr != nil || n != expectedSize(f, i) {
			return errors.New("分块读取失败，请重新上传")
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return errors.New("完整文件 SHA-256 不匹配，请重新选择原文件上传")
	}
	if err = temp.Sync(); err == nil {
		err = temp.Close()
	}
	if err == nil {
		err = os.Rename(temp.Name(), filepath.Join(a.fileDir(room, f), "content"))
	}
	if err != nil {
		return errors.New("文件合并失败，请检查磁盘空间")
	}
	return nil
}
func (a *app) download(w http.ResponseWriter, r *http.Request) {
	room := a.authorize(w, r, false)
	if room == nil {
		return
	}
	f := findFile(room, r.PathValue("file"))
	if f == nil || !f.Ready {
		room.mu.Unlock()
		fail(w, 404, "文件尚未就绪")
		return
	}
	file, err := os.Open(filepath.Join(a.fileDir(room, f), "content"))
	if err != nil {
		room.mu.Unlock()
		fail(w, 404, "文件内容不存在")
		return
	}
	name, hash := f.Name, f.SHA256
	room.mu.Unlock()
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		fail(w, 500, "文件读取失败")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("ETag", `"`+hash+`"`)
	w.Header().Set("X-File-SHA256", hash)
	http.ServeContent(w, r, name, info.ModTime(), file)
}
