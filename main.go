// Inventra — offline inventory management desktop app.
// One executable: embedded web UI + local server + JSON data store.
package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const appName = "Inventra"
const appVersion = "1.0.0"
const defaultPort = 47821

//go:embed web
var webFS embed.FS

// ---------- configuration ----------

type Config struct {
	Port int  `json:"port"`
	LAN  bool `json:"lan"`
}

var (
	dataDir string
	cfg     Config
	store   *Store
)

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	p, _ = filepath.EvalSymlinks(p)
	return filepath.Dir(p)
}

func writable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".wtest")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func resolveDataDir(override string) string {
	if override != "" {
		os.MkdirAll(override, 0o755)
		return override
	}
	d := filepath.Join(exeDir(), appName+" Data")
	if writable(d) {
		return d
	}
	if base, err := os.UserConfigDir(); err == nil {
		d = filepath.Join(base, appName)
		if writable(d) {
			return d
		}
	}
	d = filepath.Join(os.TempDir(), appName)
	os.MkdirAll(d, 0o755)
	return d
}

func loadConfig() {
	cfg = Config{Port: defaultPort}
	b, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err == nil {
		json.Unmarshal(b, &cfg)
	}
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
}

func saveConfig() error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(filepath.Join(dataDir, "config.json"), b, 0o644)
}

// ---------- data store ----------

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Role     string `json:"role"` // admin | manager | staff | viewer
	Active   bool   `json:"active"`
	Salt     string `json:"salt"`
	Hash     string `json:"hash"`
	Created  string `json:"created"`
}

type PublicUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Active   bool   `json:"active"`
}

func (u *User) Public() PublicUser {
	return PublicUser{u.ID, u.Username, u.Name, u.Role, u.Active}
}

type Activity struct {
	ID   string `json:"id"`
	TS   string `json:"ts"`
	User string `json:"user"`
	Text string `json:"text"`
}

type DBFile struct {
	Version     int                                   `json:"version"`
	Rev         int64                                 `json:"rev"`
	Collections map[string]map[string]json.RawMessage `json:"collections"`
	Users       []*User                               `json:"users"`
	Activity    []Activity                            `json:"activity"`
}

type Store struct {
	mu         sync.Mutex
	path       string
	db         DBFile
	lastBackup time.Time
}

// Collections the UI is allowed to write.
var allowedCollections = map[string]bool{
	"settings": true, "items": true, "warehouses": true, "stockIn": true, "stockOut": true,
	"adjustments": true, "transfers": true, "assemblies": true, "stockCounts": true,
}

func newStore(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "inventra-data.json")}
	b, err := os.ReadFile(s.path)
	if err == nil {
		if err := json.Unmarshal(b, &s.db); err != nil {
			return nil, fmt.Errorf("data file is damaged (%v). Restore a copy from the backups folder", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s.db.Collections == nil {
		s.db.Collections = map[string]map[string]json.RawMessage{}
	}
	if s.db.Version == 0 {
		s.db.Version = 1
	}
	s.backup(true)
	s.migrateInventoryOnly()
	return s, nil
}

func (s *Store) saveLocked() error {
	s.db.Rev++
	b, err := json.Marshal(s.db)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.backup(false)
	return nil
}

// backup keeps one copy per day, plus the 30 most recent days.
func (s *Store) backup(force bool) {
	if !force && time.Since(s.lastBackup) < time.Hour {
		return
	}
	if _, err := os.Stat(s.path); err != nil {
		return
	}
	dir := filepath.Join(filepath.Dir(s.path), "backups")
	os.MkdirAll(dir, 0o755)
	src, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	name := filepath.Join(dir, "inventra-"+time.Now().Format("2006-01-02")+".json")
	os.WriteFile(name, src, 0o644)
	s.lastBackup = time.Now()
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "inventra-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > 30 {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

func (s *Store) logLocked(user, text string) {
	if text == "" {
		return
	}
	s.db.Activity = append(s.db.Activity, Activity{ID: newID(), TS: time.Now().Format(time.RFC3339), User: user, Text: text})
	if n := len(s.db.Activity); n > 5000 {
		s.db.Activity = s.db.Activity[n-5000:]
	}
}

func (s *Store) userByName(name string) *User {
	for _, u := range s.db.Users {
		if strings.EqualFold(u.Username, name) {
			return u
		}
	}
	return nil
}

func (s *Store) userByID(id string) *User {
	for _, u := range s.db.Users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

// ---------- auth ----------

func newID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashPassword(pw, salt string) string {
	k, err := pbkdf2.Key(sha256.New, pw, []byte(salt), 120000, 32)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(k)
}

func setPassword(u *User, pw string) {
	u.Salt = newID()
	u.Hash = hashPassword(pw, u.Salt)
}

func checkPassword(u *User, pw string) bool {
	h := hashPassword(pw, u.Salt)
	return subtle.ConstantTimeCompare([]byte(h), []byte(u.Hash)) == 1
}

type session struct {
	userID string
	seen   time.Time
}

var (
	sessMu   sync.Mutex
	sessions = map[string]*session{}
)

func currentUser(r *http.Request) *User {
	tok := r.Header.Get("X-Token")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	sessMu.Lock()
	ss := sessions[tok]
	if ss != nil {
		ss.seen = time.Now()
	}
	sessMu.Unlock()
	if ss == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	u := store.userByID(ss.userID)
	if u == nil || !u.Active {
		return nil
	}
	return u
}

func roleRank(role string) int {
	switch role {
	case "admin":
		return 4
	case "manager":
		return 3
	case "staff":
		return 2
	case "viewer":
		return 1
	}
	return 0
}

// ---------- heartbeat (to exit when window closes) ----------

var (
	hbMu   sync.Mutex
	lastHB = time.Now()
)

func touchHB() {
	hbMu.Lock()
	lastHB = time.Now()
	hbMu.Unlock()
}

func sinceHB() time.Duration {
	hbMu.Lock()
	defer hbMu.Unlock()
	return time.Since(lastHB)
}

// ---------- HTTP ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readBody(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 200<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func needUser(minRole string, h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil {
			fail(w, 401, "Please sign in again.")
			return
		}
		if roleRank(u.Role) < roleRank(minRole) {
			fail(w, 403, "Your role doesn't allow this action.")
			return
		}
		touchHB()
		h(w, r, u)
	}
}

func handlers() http.Handler {
	mux := http.NewServeMux()

	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"app": appName, "version": appVersion})
	})

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		store.mu.Lock()
		n := len(store.db.Users)
		company := ""
		if raw, ok := store.db.Collections["settings"]["main"]; ok {
			var st struct {
				Company struct {
					Name string `json:"name"`
				} `json:"company"`
			}
			json.Unmarshal(raw, &st)
			company = st.Company.Name
		}
		store.mu.Unlock()
		writeJSON(w, 200, map[string]any{"setupNeeded": n == 0, "company": company, "version": appVersion})
	})

	mux.HandleFunc("POST /api/setup", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Username, Password, Name string
			Settings                 json.RawMessage
			Seed                     map[string][]json.RawMessage
		}
		if err := readBody(r, &in); err != nil {
			fail(w, 400, "Bad request")
			return
		}
		in.Username = strings.TrimSpace(in.Username)
		if in.Username == "" || len(in.Password) < 4 {
			fail(w, 400, "Enter a username and a password of at least 4 characters.")
			return
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if len(store.db.Users) > 0 {
			fail(w, 400, "Setup has already been completed.")
			return
		}
		u := &User{ID: newID(), Username: in.Username, Name: in.Name, Role: "admin", Active: true, Created: time.Now().Format(time.RFC3339)}
		setPassword(u, in.Password)
		store.db.Users = append(store.db.Users, u)
		if len(in.Settings) > 0 {
			store.put("settings", "main", in.Settings)
		}
		for c, docs := range in.Seed {
			if !allowedCollections[c] {
				continue
			}
			for _, d := range docs {
				var probe struct{ ID string }
				if json.Unmarshal(d, &probe) == nil && probe.ID != "" {
					store.put(c, probe.ID, d)
				}
			}
		}
		store.logLocked(u.Username, "Set up the company and created the admin account")
		if err := store.saveLocked(); err != nil {
			fail(w, 500, "Could not save: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Username, Password string }
		if err := readBody(r, &in); err != nil {
			fail(w, 400, "Bad request")
			return
		}
		store.mu.Lock()
		u := store.userByName(strings.TrimSpace(in.Username))
		store.mu.Unlock()
		if u == nil || !u.Active || !checkPassword(u, in.Password) {
			time.Sleep(400 * time.Millisecond)
			fail(w, 401, "Wrong username or password.")
			return
		}
		tok := newID() + newID()
		sessMu.Lock()
		sessions[tok] = &session{userID: u.ID, seen: time.Now()}
		sessMu.Unlock()
		store.mu.Lock()
		store.logLocked(u.Username, "Signed in")
		store.saveLocked()
		store.mu.Unlock()
		touchHB()
		writeJSON(w, 200, map[string]any{"token": tok, "user": u.Public()})
	})

	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		sessMu.Lock()
		delete(sessions, r.Header.Get("X-Token"))
		sessMu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		touchHB()
		store.mu.Lock()
		rev := store.db.Rev
		store.mu.Unlock()
		writeJSON(w, 200, map[string]any{"rev": rev})
	})

	mux.HandleFunc("GET /api/state", needUser("viewer", func(w http.ResponseWriter, r *http.Request, u *User) {
		store.mu.Lock()
		defer store.mu.Unlock()
		out := map[string]any{}
		for c, m := range store.db.Collections {
			arr := make([]json.RawMessage, 0, len(m))
			for _, d := range m {
				arr = append(arr, d)
			}
			out[c] = arr
		}
		users := []PublicUser{}
		for _, x := range store.db.Users {
			users = append(users, x.Public())
		}
		writeJSON(w, 200, map[string]any{"rev": store.db.Rev, "data": out, "users": users, "me": u.Public()})
	}))

	mux.HandleFunc("GET /api/activity", needUser("viewer", func(w http.ResponseWriter, r *http.Request, u *User) {
		store.mu.Lock()
		defer store.mu.Unlock()
		writeJSON(w, 200, store.db.Activity)
	}))

	mux.HandleFunc("POST /api/commit", needUser("staff", func(w http.ResponseWriter, r *http.Request, u *User) {
		var in struct {
			Puts []struct {
				C   string          `json:"c"`
				Doc json.RawMessage `json:"doc"`
			} `json:"puts"`
			Dels []struct {
				C  string `json:"c"`
				ID string `json:"id"`
			} `json:"dels"`
			Log string `json:"log"`
		}
		if err := readBody(r, &in); err != nil {
			fail(w, 400, "Bad request")
			return
		}
		rank := roleRank(u.Role)
		for _, p := range in.Puts {
			if !allowedCollections[p.C] {
				fail(w, 400, "Unknown collection "+p.C)
				return
			}
			if (p.C == "settings" || p.C == "warehouses") && rank < roleRank("manager") {
				fail(w, 403, "Only managers and admins can change settings.")
				return
			}
		}
		if len(in.Dels) > 0 && rank < roleRank("manager") {
			fail(w, 403, "Only managers and admins can delete records.")
			return
		}
		type idDoc struct {
			ID string `json:"id"`
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		for _, p := range in.Puts {
			var d idDoc
			if err := json.Unmarshal(p.Doc, &d); err != nil || d.ID == "" {
				fail(w, 400, "Record without id")
				return
			}
			store.put(p.C, d.ID, p.Doc)
		}
		for _, d := range in.Dels {
			if m := store.db.Collections[d.C]; m != nil {
				delete(m, d.ID)
			}
		}
		store.logLocked(u.Username, in.Log)
		if err := store.saveLocked(); err != nil {
			fail(w, 500, "Could not save: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "rev": store.db.Rev})
	}))

	mux.HandleFunc("POST /api/users", needUser("admin", func(w http.ResponseWriter, r *http.Request, u *User) {
		var in struct {
			ID, Username, Name, Role, Password string
			Active                             bool
		}
		if err := readBody(r, &in); err != nil {
			fail(w, 400, "Bad request")
			return
		}
		in.Username = strings.TrimSpace(in.Username)
		if roleRank(in.Role) == 0 || in.Username == "" {
			fail(w, 400, "Username and role are required.")
			return
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		other := store.userByName(in.Username)
		var target *User
		if in.ID != "" {
			target = store.userByID(in.ID)
			if target == nil {
				fail(w, 404, "User not found")
				return
			}
			if other != nil && other.ID != target.ID {
				fail(w, 400, "That username is taken.")
				return
			}
		} else {
			if other != nil {
				fail(w, 400, "That username is taken.")
				return
			}
			if len(in.Password) < 4 {
				fail(w, 400, "Password must be at least 4 characters.")
				return
			}
			target = &User{ID: newID(), Created: time.Now().Format(time.RFC3339)}
			store.db.Users = append(store.db.Users, target)
		}
		// never lock out the last admin
		if target.Role == "admin" && (in.Role != "admin" || !in.Active) {
			admins := 0
			for _, x := range store.db.Users {
				if x.Role == "admin" && x.Active {
					admins++
				}
			}
			if admins <= 1 {
				fail(w, 400, "You need at least one active admin.")
				return
			}
		}
		target.Username, target.Name, target.Role, target.Active = in.Username, in.Name, in.Role, in.Active
		if in.Password != "" {
			if len(in.Password) < 4 {
				fail(w, 400, "Password must be at least 4 characters.")
				return
			}
			setPassword(target, in.Password)
		}
		store.logLocked(u.Username, "Saved user "+target.Username)
		if err := store.saveLocked(); err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, target.Public())
	}))

	mux.HandleFunc("POST /api/password", needUser("viewer", func(w http.ResponseWriter, r *http.Request, u *User) {
		var in struct{ Old, New string }
		if err := readBody(r, &in); err != nil {
			fail(w, 400, "Bad request")
			return
		}
		if !checkPassword(u, in.Old) {
			fail(w, 400, "Current password is wrong.")
			return
		}
		if len(in.New) < 4 {
			fail(w, 400, "New password must be at least 4 characters.")
			return
		}
		store.mu.Lock()
		setPassword(u, in.New)
		store.logLocked(u.Username, "Changed own password")
		err := store.saveLocked()
		store.mu.Unlock()
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("GET /api/backup", needUser("manager", func(w http.ResponseWriter, r *http.Request, u *User) {
		store.mu.Lock()
		b, _ := json.MarshalIndent(store.db, "", " ")
		store.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="inventra-backup-`+time.Now().Format("2006-01-02-1504")+`.json"`)
		w.Write(b)
	}))

	mux.HandleFunc("POST /api/restore", needUser("admin", func(w http.ResponseWriter, r *http.Request, u *User) {
		var in DBFile
		if err := readBody(r, &in); err != nil || in.Collections == nil {
			fail(w, 400, "That file is not an Inventra backup.")
			return
		}
		if len(in.Users) == 0 {
			fail(w, 400, "Backup has no users; refusing to restore.")
			return
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		store.backup(true)
		// keep a safety copy of the current data
		cur, _ := json.Marshal(store.db)
		os.WriteFile(filepath.Join(dataDir, "backups", "before-restore-"+time.Now().Format("2006-01-02-150405")+".json"), cur, 0o644)
		rev := store.db.Rev
		store.db = in
		store.db.Rev = rev
		store.logLocked(u.Username, "Restored data from a backup file")
		if err := store.saveLocked(); err != nil {
			fail(w, 500, err.Error())
			return
		}
		sessMu.Lock()
		sessions = map[string]*session{}
		sessMu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("POST /api/reset", needUser("admin", func(w http.ResponseWriter, r *http.Request, u *User) {
		var in struct{ Password string }
		readBody(r, &in)
		if !checkPassword(u, in.Password) {
			fail(w, 400, "Password is wrong.")
			return
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		store.backup(true)
		cur, _ := json.Marshal(store.db)
		os.WriteFile(filepath.Join(dataDir, "backups", "before-reset-"+time.Now().Format("2006-01-02-150405")+".json"), cur, 0o644)
		store.db.Collections = map[string]map[string]json.RawMessage{
			"settings":   store.db.Collections["settings"],
			"warehouses": store.db.Collections["warehouses"],
		}
		store.logLocked(u.Username, "Erased all items and stock records")
		store.saveLocked()
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("GET /api/info", needUser("viewer", func(w http.ResponseWriter, r *http.Request, u *User) {
		var backups []string
		entries, _ := os.ReadDir(filepath.Join(dataDir, "backups"))
		for _, e := range entries {
			backups = append(backups, e.Name())
		}
		sort.Sort(sort.Reverse(sort.StringSlice(backups)))
		writeJSON(w, 200, map[string]any{
			"dataDir": dataDir, "backups": backups, "lan": cfg.LAN, "port": cfg.Port,
			"addresses": lanAddresses(), "version": appVersion,
		})
	}))

	mux.HandleFunc("POST /api/config", needUser("admin", func(w http.ResponseWriter, r *http.Request, u *User) {
		var in struct{ LAN bool }
		if err := readBody(r, &in); err != nil {
			fail(w, 400, "Bad request")
			return
		}
		cfg.LAN = in.LAN
		if err := saveConfig(); err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "restartNeeded": true})
	}))

	return mux
}

func (s *Store) put(c, id string, doc json.RawMessage) {
	m := s.db.Collections[c]
	if m == nil {
		m = map[string]json.RawMessage{}
		s.db.Collections[c] = m
	}
	cp := make(json.RawMessage, len(doc))
	copy(cp, doc)
	m[id] = cp
}

func lanAddresses() []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

// ---------- startup ----------

func alreadyRunning(port int) bool {
	c := http.Client{Timeout: 800 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var p map[string]string
	json.NewDecoder(resp.Body).Decode(&p)
	return p["app"] == appName
}

func main() {
	noBrowser := flag.Bool("nobrowser", false, "only run the server")
	dirFlag := flag.String("data", "", "data folder")
	flag.Parse()

	dataDir = resolveDataDir(*dirFlag)
	logf, err := os.OpenFile(filepath.Join(dataDir, "inventra.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		log.SetOutput(io.MultiWriter(logf, os.Stderr))
	}
	loadConfig()

	if alreadyRunning(cfg.Port) {
		if !*noBrowser {
			openWindow(fmt.Sprintf("http://127.0.0.1:%d/", cfg.Port), dataDir)
		}
		return
	}

	store, err = newStore(dataDir)
	if err != nil {
		fatal(err.Error())
	}

	host := "127.0.0.1"
	if cfg.LAN {
		host = "0.0.0.0"
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, cfg.Port))
	if err != nil {
		ln, err = net.Listen("tcp", host+":0")
		if err != nil {
			fatal("Could not start: " + err.Error())
		}
		cfg.Port = ln.Addr().(*net.TCPAddr).Port
		saveConfig()
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", cfg.Port)
	log.Printf("%s %s running at %s (data: %s)", appName, appVersion, url, dataDir)

	srv := &http.Server{Handler: handlers(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal(err.Error())
		}
	}()

	if *noBrowser {
		select {}
	}

	touchHB()
	waited := openWindow(url, dataDir)
	// After the app window closes (or for a normal browser tab, when no page
	// has checked in for a while), shut down. With LAN sharing on, keep running
	// while other computers are still using it.
	idle := 3 * time.Minute
	if waited {
		idle = 45 * time.Second
	}
	if waited && !cfg.LAN {
		idle = 3 * time.Second
	}
	for {
		time.Sleep(2 * time.Second)
		if sinceHB() > idle {
			break
		}
	}
	store.mu.Lock()
	store.backup(true)
	store.mu.Unlock()
	log.Printf("shutting down")
}

// migrateInventoryOnly upgrades data from the first (sales + purchases) build:
// it drops the billing collections and the sample items that build could seed.
func (s *Store) migrateInventoryOnly() {
	if s.db.Version >= 2 {
		return
	}
	changed := false
	for c := range s.db.Collections {
		if !allowedCollections[c] {
			delete(s.db.Collections, c)
			changed = true
		}
	}
	demo := map[string]bool{"LED-12W": true, "LED-T20": true, "WIRE-729": true, "SW-2G": true, "CB-32A": true, "PVC-20": true, "FAN-56": true, "EXT-4W": true}
	for id, raw := range s.db.Collections["items"] {
		var it struct {
			SKU       string `json:"sku"`
			CreatedBy string `json:"createdBy"`
		}
		if json.Unmarshal(raw, &it) == nil && demo[it.SKU] && it.CreatedBy == "" {
			delete(s.db.Collections["items"], id)
			changed = true
		}
	}
	s.db.Version = 2
	if changed {
		s.logLocked("system", "Upgraded to the inventory-only version (removed sample data and billing records)")
	}
	s.saveLocked()
}
