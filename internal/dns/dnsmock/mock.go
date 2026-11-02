// Package dnsmock is an in-memory fake of the subset of the Cloudflare v4 and ArvanCloud CDN 4.0
// DNS APIs that Intrabin uses. It is for tests and local demos only; it never talks to a real
// provider. Routes: /cloudflare/client/v4/... and /arvan/cdn/4.0/...; control endpoints under
// /_mock/ (reset, seed, fault injection, write counters).
package dnsmock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is the fake provider.
type Server struct {
	CFToken  string
	ArvanKey string

	mu      sync.Mutex
	seq     int
	zones   map[string]*zone // by zone name
	writes  map[string]int   // method -> count of applied writes
	faults  []fault
	hangFor time.Duration
}

type zone struct {
	id, name string
	records  map[string]map[string]any // id -> record (Cloudflare shape or Arvan shape)
	provider string                    // "cloudflare" | "arvan"
}

type fault struct {
	provider, method string
	status           int
	applyFirst       bool // apply the write, then fail (simulates a lost response)
	hang             bool // apply the write, then never answer
}

// New returns a mock with the given credentials.
func New(cfToken, arvanKey string) *Server {
	s := &Server{CFToken: cfToken, ArvanKey: arvanKey}
	s.Reset()
	return s
}

// Reset clears all zones, faults and counters.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.zones, s.writes, s.faults = map[string]*zone{}, map[string]int{}, nil
}

func (s *Server) nextID(p string) string {
	s.seq++
	if p == "cloudflare" {
		return fmt.Sprintf("%032x", s.seq)
	}
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", s.seq)
}

// AddZone creates a zone for provider ("cloudflare" or "arvan").
func (s *Server) AddZone(provider, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := &zone{id: s.nextID("cloudflare"), name: name, records: map[string]map[string]any{}, provider: provider}
	s.zones[provider+"/"+name] = z
	return z.id
}

// AddRecord seeds a record (A/AAAA/CNAME/TXT/MX). content is an IP, host or text.
func (s *Server) AddRecord(provider, zoneName, typ, name, content string, ttl int, proxied bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := s.zones[provider+"/"+zoneName]
	id := s.nextID(provider)
	if provider == "cloudflare" {
		fq := name
		if name == "@" || name == "" {
			fq = zoneName
		} else if !strings.HasSuffix(name, zoneName) {
			fq = name + "." + zoneName
		}
		z.records[id] = map[string]any{"id": id, "type": typ, "name": fq, "content": content, "ttl": float64(ttl), "proxied": proxied}
	} else {
		var val any
		switch typ {
		case "A", "AAAA":
			var vs []any
			for _, ip := range strings.Split(content, ",") {
				vs = append(vs, map[string]any{"ip": ip, "port": nil, "weight": nil, "country": ""})
			}
			val = vs
		case "CNAME":
			val = map[string]any{"host": content, "host_header": "source"}
		case "MX":
			val = map[string]any{"host": content, "priority": float64(10)}
		default:
			val = map[string]any{"text": content}
		}
		z.records[id] = map[string]any{"id": id, "type": strings.ToLower(typ), "name": name, "value": val, "ttl": float64(ttl), "cloud": proxied,
			"upstream_https": "default", "ip_filter_mode": map[string]any{"count": "single", "order": "none", "geo_filter": "none"}}
	}
	return id
}

// Fail makes the next write with method (POST/PUT/DELETE) to provider fail with status. If
// applyFirst, the write is applied before the error is returned (a lost response).
func (s *Server) Fail(provider, method string, status int, applyFirst bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, fault{provider: provider, method: method, status: status, applyFirst: applyFirst})
}

// Hang makes the next write to provider apply and then hold the response for d.
func (s *Server) Hang(provider, method string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, fault{provider: provider, method: method, hang: true})
	s.hangFor = d
}

// Writes returns how many writes were applied per "provider METHOD".
func (s *Server) Writes() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.writes {
		out[k] = v
	}
	return out
}

// Records returns a copy of a zone's records in provider shape.
func (s *Server) Records(provider, zoneName string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	z := s.zones[provider+"/"+zoneName]
	if z == nil {
		return nil
	}
	var out []map[string]any
	for _, r := range z.records {
		c := map[string]any{}
		for k, v := range r {
			c[k] = v
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return out
}

func (s *Server) takeFault(provider, method string) *fault {
	for i, f := range s.faults {
		if f.provider == provider && f.method == method {
			s.faults = append(s.faults[:i], s.faults[i+1:]...)
			return &f
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ServeHTTP routes requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/cloudflare/client/v4/"):
		s.cloudflare(w, r, strings.TrimPrefix(r.URL.Path, "/cloudflare/client/v4"))
	case strings.HasPrefix(r.URL.Path, "/arvan/cdn/4.0/"):
		s.arvan(w, r, strings.TrimPrefix(r.URL.Path, "/arvan/cdn/4.0"))
	case strings.HasPrefix(r.URL.Path, "/_mock/"):
		s.control(w, r)
	default:
		http.NotFound(w, r)
	}
}

// control: POST /_mock/reset, POST /_mock/zones {provider,name,records:[{type,name,content,ttl,proxied}]},
// POST /_mock/faults {provider,method,status,apply_first,hang_seconds}, GET /_mock/state.
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	switch r.Method + " " + r.URL.Path {
	case "POST /_mock/reset":
		s.Reset()
		writeJSON(w, 200, map[string]any{"ok": true})
	case "POST /_mock/zones":
		var in struct {
			Provider string `json:"provider"`
			Name     string `json:"name"`
			Records  []struct {
				Type, Name, Content string
				TTL                 int
				Proxied             bool
			} `json:"records"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			writeJSON(w, 400, map[string]any{"error": "bad json"})
			return
		}
		s.AddZone(in.Provider, in.Name)
		for _, rec := range in.Records {
			s.AddRecord(in.Provider, in.Name, rec.Type, rec.Name, rec.Content, rec.TTL, rec.Proxied)
		}
		writeJSON(w, 201, map[string]any{"ok": true})
	case "POST /_mock/faults":
		var in struct {
			Provider    string `json:"provider"`
			Method      string `json:"method"`
			Status      int    `json:"status"`
			ApplyFirst  bool   `json:"apply_first"`
			HangSeconds int    `json:"hang_seconds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.HangSeconds > 0 {
			s.Hang(in.Provider, in.Method, time.Duration(in.HangSeconds)*time.Second)
		} else {
			s.Fail(in.Provider, in.Method, in.Status, in.ApplyFirst)
		}
		writeJSON(w, 201, map[string]any{"ok": true})
	case "GET /_mock/state":
		s.mu.Lock()
		zs := map[string]any{}
		for k, z := range s.zones {
			zs[k] = len(z.records)
		}
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"zones": zs, "writes": s.Writes()})
	default:
		http.NotFound(w, r)
	}
}

func page(r *http.Request, def int) (int, int) {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pp, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if p < 1 {
		p = 1
	}
	if pp < 1 {
		pp = def
	}
	return p, pp
}

func paginate[T any](items []T, p, pp int) ([]T, int) {
	pages := (len(items) + pp - 1) / pp
	if pages == 0 {
		pages = 1
	}
	lo := (p - 1) * pp
	if lo > len(items) {
		lo = len(items)
	}
	hi := min(lo+pp, len(items))
	return items[lo:hi], pages
}

// applyWrite runs fn under the lock, honouring injected faults. It returns false if a response
// has already been written.
func (s *Server) applyWrite(w http.ResponseWriter, provider, method string, fn func() (int, any)) {
	s.mu.Lock()
	f := s.takeFault(provider, method)
	hang := s.hangFor
	if f != nil && !f.applyFirst && !f.hang {
		s.mu.Unlock()
		writeJSON(w, f.status, errorBody(provider, f.status))
		return
	}
	status, body := fn()
	if status < 300 {
		s.writes[provider+" "+method]++
	}
	s.mu.Unlock()
	if f != nil && f.hang {
		time.Sleep(hang)
		writeJSON(w, status, body)
		return
	}
	if f != nil && f.applyFirst {
		writeJSON(w, f.status, errorBody(provider, f.status))
		return
	}
	writeJSON(w, status, body)
}

func errorBody(provider string, status int) any {
	if provider == "cloudflare" {
		return map[string]any{"success": false, "errors": []any{map[string]any{"code": 10000 + status, "message": http.StatusText(status)}}, "messages": []any{}, "result": nil}
	}
	return map[string]any{"message": http.StatusText(status)}
}

// ---- Cloudflare ----

func cfOK(result any, info map[string]any) map[string]any {
	m := map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result}
	if info != nil {
		m["result_info"] = info
	}
	return m
}

func (s *Server) cloudflare(w http.ResponseWriter, r *http.Request, path string) {
	if r.Header.Get("Authorization") != "Bearer "+s.CFToken {
		writeJSON(w, 401, map[string]any{"success": false, "errors": []any{map[string]any{"code": 10000, "message": "Authentication error"}}, "messages": []any{}, "result": nil})
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/user/tokens/verify" && r.Method == "GET":
		writeJSON(w, 200, cfOK(map[string]any{"id": "mock-token", "status": "active"}, nil))
	case path == "/zones" && r.Method == "GET":
		s.mu.Lock()
		var zs []map[string]any
		for _, z := range s.zones {
			if z.provider == "cloudflare" {
				zs = append(zs, map[string]any{"id": z.id, "name": z.name, "status": "active"})
			}
		}
		s.mu.Unlock()
		sort.Slice(zs, func(i, j int) bool { return zs[i]["name"].(string) < zs[j]["name"].(string) })
		p, pp := page(r, 20)
		items, pages := paginate(zs, p, pp)
		writeJSON(w, 200, cfOK(items, map[string]any{"page": p, "per_page": pp, "total_pages": pages, "count": len(items), "total_count": len(zs)}))
	case len(parts) >= 3 && parts[0] == "zones" && parts[2] == "dns_records":
		s.mu.Lock()
		var z *zone
		for _, zz := range s.zones {
			if zz.provider == "cloudflare" && zz.id == parts[1] {
				z = zz
			}
		}
		s.mu.Unlock()
		if z == nil {
			writeJSON(w, 404, errorBody("cloudflare", 404))
			return
		}
		s.cfRecords(w, r, z, parts[3:])
	default:
		writeJSON(w, 404, errorBody("cloudflare", 404))
	}
}

func (s *Server) cfRecords(w http.ResponseWriter, r *http.Request, z *zone, rest []string) {
	decode := func() (map[string]any, bool) {
		var in map[string]any
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			writeJSON(w, 400, errorBody("cloudflare", 400))
			return nil, false
		}
		name, _ := in["name"].(string)
		if name == "@" || name == "" {
			in["name"] = z.name
		} else if !strings.HasSuffix(name, z.name) {
			in["name"] = name + "." + z.name
		}
		if _, ok := in["ttl"]; !ok {
			in["ttl"] = float64(1)
		}
		return in, true
	}
	switch {
	case len(rest) == 0 && r.Method == "GET":
		s.mu.Lock()
		var rs []map[string]any
		for _, rec := range z.records {
			rs = append(rs, rec)
		}
		s.mu.Unlock()
		sort.Slice(rs, func(i, j int) bool { return rs[i]["id"].(string) < rs[j]["id"].(string) })
		p, pp := page(r, 100)
		items, pages := paginate(rs, p, pp)
		writeJSON(w, 200, cfOK(items, map[string]any{"page": p, "per_page": pp, "total_pages": pages, "count": len(items), "total_count": len(rs)}))
	case len(rest) == 0 && r.Method == "POST":
		in, ok := decode()
		if !ok {
			return
		}
		s.applyWrite(w, "cloudflare", "POST", func() (int, any) {
			for _, rec := range z.records {
				if rec["type"] == in["type"] && rec["name"] == in["name"] && rec["content"] == in["content"] {
					return 400, map[string]any{"success": false, "errors": []any{map[string]any{"code": 81058, "message": "An identical record already exists."}}, "messages": []any{}, "result": nil}
				}
			}
			id := s.nextID("cloudflare")
			in["id"] = id
			z.records[id] = in
			return 200, cfOK(in, nil)
		})
	case len(rest) == 1 && (r.Method == "PUT" || r.Method == "DELETE"):
		id := rest[0]
		var in map[string]any
		if r.Method == "PUT" {
			var ok bool
			if in, ok = decode(); !ok {
				return
			}
		}
		s.applyWrite(w, "cloudflare", r.Method, func() (int, any) {
			if _, ok := z.records[id]; !ok {
				return 404, map[string]any{"success": false, "errors": []any{map[string]any{"code": 81044, "message": "Record does not exist."}}, "messages": []any{}, "result": nil}
			}
			if r.Method == "DELETE" {
				delete(z.records, id)
				return 200, cfOK(map[string]any{"id": id}, nil)
			}
			in["id"] = id
			z.records[id] = in
			return 200, cfOK(in, nil)
		})
	default:
		writeJSON(w, 404, errorBody("cloudflare", 404))
	}
}

// ---- ArvanCloud ----

func (s *Server) arvan(w http.ResponseWriter, r *http.Request, path string) {
	if r.Header.Get("Authorization") != "Apikey "+s.ArvanKey {
		writeJSON(w, 401, map[string]any{"message": "Unauthenticated."})
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "domains" && r.Method == "GET":
		s.mu.Lock()
		var ds []map[string]any
		for _, z := range s.zones {
			if z.provider == "arvan" {
				ds = append(ds, map[string]any{"id": z.id, "domain": z.name, "name": z.name, "status": "active"})
			}
		}
		s.mu.Unlock()
		sort.Slice(ds, func(i, j int) bool { return ds[i]["domain"].(string) < ds[j]["domain"].(string) })
		p, pp := page(r, 15)
		items, pages := paginate(ds, p, pp)
		writeJSON(w, 200, map[string]any{"data": items, "meta": map[string]any{"current_page": p, "last_page": pages, "per_page": pp, "total": len(ds)}})
	case len(parts) >= 3 && parts[0] == "domains" && parts[2] == "dns-records":
		s.mu.Lock()
		z := s.zones["arvan/"+parts[1]]
		s.mu.Unlock()
		if z == nil {
			writeJSON(w, 404, map[string]any{"message": "Domain not found"})
			return
		}
		s.arRecords(w, r, z, parts[3:])
	default:
		writeJSON(w, 404, map[string]any{"message": "Not found"})
	}
}

func (s *Server) arRecords(w http.ResponseWriter, r *http.Request, z *zone, rest []string) {
	decode := func() (map[string]any, bool) {
		var in map[string]any
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			writeJSON(w, 422, map[string]any{"message": "The given data was invalid."})
			return nil, false
		}
		if _, ok := in["value"]; !ok {
			writeJSON(w, 422, map[string]any{"message": "The given data was invalid.", "errors": map[string]any{"value": []any{"The value field is required."}}})
			return nil, false
		}
		return in, true
	}
	switch {
	case len(rest) == 0 && r.Method == "GET":
		s.mu.Lock()
		var rs []map[string]any
		for _, rec := range z.records {
			rs = append(rs, rec)
		}
		s.mu.Unlock()
		sort.Slice(rs, func(i, j int) bool { return rs[i]["id"].(string) < rs[j]["id"].(string) })
		p, pp := page(r, 15)
		items, pages := paginate(rs, p, pp)
		writeJSON(w, 200, map[string]any{"data": items, "meta": map[string]any{"current_page": p, "last_page": pages, "per_page": pp, "total": len(rs)}})
	case len(rest) == 0 && r.Method == "POST":
		in, ok := decode()
		if !ok {
			return
		}
		s.applyWrite(w, "arvan", "POST", func() (int, any) {
			id := s.nextID("arvan")
			in["id"] = id
			z.records[id] = in
			return 201, map[string]any{"message": "DNS record created", "data": in}
		})
	case len(rest) == 1 && (r.Method == "PUT" || r.Method == "DELETE"):
		id := rest[0]
		var in map[string]any
		if r.Method == "PUT" {
			var ok bool
			if in, ok = decode(); !ok {
				return
			}
		}
		s.applyWrite(w, "arvan", r.Method, func() (int, any) {
			if _, ok := z.records[id]; !ok {
				return 404, map[string]any{"message": "DNS record not found"}
			}
			if r.Method == "DELETE" {
				delete(z.records, id)
				return 200, map[string]any{"message": "DNS record deleted"}
			}
			in["id"] = id
			z.records[id] = in
			return 200, map[string]any{"message": "DNS record updated", "data": in}
		})
	default:
		writeJSON(w, 404, map[string]any{"message": "Not found"})
	}
}
