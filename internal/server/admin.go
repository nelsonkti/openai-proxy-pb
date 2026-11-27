package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"openai-proxy/internal/proxy"
)

type errorEvent struct {
	At        time.Time `json:"at"`
	RequestID string    `json:"request_id,omitempty"`
	Method    string    `json:"method,omitempty"`
	Path      string    `json:"path,omitempty"`
	Status    int       `json:"status,omitempty"`
	Message   string    `json:"message"`
}

type errorLogBuffer struct {
	mu      sync.RWMutex
	entries []errorEvent
	limit   int
}

const adminSessionCookie = "openai_proxy_admin"

type adminSessionStore struct {
	mu      sync.Mutex
	tokens  map[string]time.Time
	expires time.Duration
}

func newAdminSessionStore() *adminSessionStore {
	return &adminSessionStore{tokens: make(map[string]time.Time), expires: 12 * time.Hour}
}

func (s *adminSessionStore) create() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(random[:])
	s.mu.Lock()
	s.tokens[token] = time.Now().Add(s.expires)
	s.mu.Unlock()
	return token, nil
}

func (s *adminSessionStore) valid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.tokens[token]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(s.tokens, token)
		return false
	}
	return true
}

func (s *adminSessionStore) revoke(token string) {
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

func newErrorLogBuffer(limit int) *errorLogBuffer { return &errorLogBuffer{limit: limit} }

func (b *errorLogBuffer) add(event errorEvent) {
	event.At = time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = append(b.entries, event)
	if len(b.entries) > b.limit {
		b.entries = b.entries[len(b.entries)-b.limit:]
	}
}

func (b *errorLogBuffer) list() []errorEvent {
	b.mu.RLock()
	defer b.mu.RUnlock()
	entries := make([]errorEvent, len(b.entries))
	copy(entries, b.entries)
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	return entries
}

func (s *Server) recordError(event errorEvent) {
	event.Message = s.sanitizeMessage(event.Message)
	if s.errors != nil {
		s.errors.add(event)
	}
}

func (s *Server) sanitizeMessage(message string) string {
	for _, key := range s.config.Keys {
		for _, secret := range []string{key.Key, key.UpstreamKey} {
			secret = strings.TrimSpace(secret)
			if secret != "" {
				message = strings.ReplaceAll(message, secret, proxy.MaskSecret(secret))
			}
		}
	}
	if s.config.Admin.APIKey != "" {
		message = strings.ReplaceAll(message, s.config.Admin.APIKey, proxy.MaskSecret(s.config.Admin.APIKey))
	}
	return message
}

func (s *Server) isAdminPath(path string) bool {
	return path == s.config.Admin.Path || strings.HasPrefix(path, s.config.Admin.Path+"/")
}

func (s *Server) handleAdmin(writer http.ResponseWriter, request *http.Request) {
	relative := strings.TrimPrefix(request.URL.Path, s.config.Admin.Path)
	if relative == "" || relative == "/" {
		if request.Method != http.MethodGet {
			writeAPIError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "HTTP method is not supported", nil)
			return
		}
		s.writeAdminPage(writer)
		return
	}
	if relative == "/api/login" && request.Method == http.MethodPost {
		s.handleAdminLogin(writer, request)
		return
	}
	if relative == "/api/logout" && request.Method == http.MethodPost {
		s.handleAdminLogout(writer, request)
		return
	}
	if !s.authorizeAdmin(request) {
		writeAPIError(writer, http.StatusUnauthorized, "invalid_api_key", "Invalid admin API key", nil)
		return
	}
	switch {
	case relative == "/api/status" && request.Method == http.MethodGet:
		s.writeAdminStatus(writer)
	case relative == "/api/logs" && request.Method == http.MethodGet:
		writeJSON(writer, http.StatusOK, s.errors.list())
	case strings.HasPrefix(relative, "/api/keys/") && request.Method == http.MethodPost:
		value := strings.TrimPrefix(relative, "/api/keys/")
		if strings.HasSuffix(value, "/test") {
			s.handleAdminKeyTest(writer, strings.TrimSuffix(value, "/test"), request)
			return
		}
		s.handleAdminKeyAction(writer, value)
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "Admin endpoint not found", nil)
	}
}

func (s *Server) authorizeAdmin(request *http.Request) bool {
	provided := strings.TrimSpace(request.Header.Get("X-Admin-Key"))
	if provided == "" {
		value := strings.TrimSpace(request.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(value), "bearer ") {
			provided = strings.TrimSpace(value[7:])
		}
	}
	if provided == "" || s.config.Admin.APIKey == "" {
		if s.sessions == nil {
			return false
		}
		cookie, err := request.Cookie(adminSessionCookie)
		return err == nil && s.sessions.valid(cookie.Value)
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.config.Admin.APIKey)) == 1
}

func (s *Server) handleAdminLogin(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 64*1024)).Decode(&payload); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid login request", nil)
		return
	}
	if s.config.Admin.APIKey == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(payload.APIKey)), []byte(s.config.Admin.APIKey)) != 1 {
		writeAPIError(writer, http.StatusUnauthorized, "invalid_api_key", "Invalid admin API key", nil)
		return
	}
	token, err := s.sessions.create()
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "internal_server_error", "Unable to create admin session", nil)
		return
	}
	http.SetCookie(writer, &http.Cookie{Name: adminSessionCookie, Value: token, Path: s.config.Admin.Path, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(s.sessions.expires.Seconds())})
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminLogout(writer http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(adminSessionCookie); err == nil && s.sessions != nil {
		s.sessions.revoke(cookie.Value)
	}
	http.SetCookie(writer, &http.Cookie{Name: adminSessionCookie, Value: "", Path: s.config.Admin.Path, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) writeAdminStatus(writer http.ResponseWriter) {
	statuses := make([]proxy.KeyStatus, 0)
	for _, pool := range s.pools {
		statuses = append(statuses, pool.Statuses()...)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"keys": statuses, "error_count": len(s.errors.list())})
}

func (s *Server) handleAdminKeyAction(writer http.ResponseWriter, value string) {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) != 2 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Expected /api/keys/{id}/{enable|disable}", nil)
		return
	}
	id, err := strconv.Atoi(parts[0])
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid key ID", nil)
		return
	}
	changed := false
	for _, pool := range s.pools {
		switch parts[1] {
		case "enable":
			changed = pool.Enable(id) || changed
		case "disable":
			changed = pool.Disable(id, "manually disabled") || changed
		default:
			writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Unsupported key action", nil)
			return
		}
	}
	if !changed {
		writeAPIError(writer, http.StatusNotFound, "not_found", "Key ID not found or already in requested state", nil)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminKeyTest(writer http.ResponseWriter, value string, request *http.Request) {
	id, err := strconv.Atoi(strings.Trim(value, "/"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid key ID", nil)
		return
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 64*1024)).Decode(&payload); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid test request", nil)
		return
	}
	for _, pool := range s.pools {
		response, found, testErr := pool.Test(request.Context(), id, payload.Model)
		if !found {
			continue
		}
		if testErr != nil {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", testErr.Error(), nil)
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		message := s.sanitizeMessage(strings.TrimSpace(string(body)))
		if len(message) > 1000 {
			message = message[:1000]
		}
		success := response.StatusCode >= 200 && response.StatusCode < 300
		if response.StatusCode >= 400 {
			s.recordError(errorEvent{Method: http.MethodGet, Path: "/v1/models/" + payload.Model, Status: response.StatusCode, Message: "admin key test: " + message})
		}
		writeJSON(writer, http.StatusOK, map[string]any{"ok": success, "status": response.StatusCode, "model": strings.TrimSpace(payload.Model), "response": message})
		return
	}
	writeAPIError(writer, http.StatusNotFound, "not_found", "Key ID not found", nil)
}

func (s *Server) writeAdminPage(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, adminPage)
}

func responseErrorDetails(response *http.Response) string {
	if response == nil {
		return "upstream error"
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Sprintf("upstream status %d", response.StatusCode)
	}
	_ = response.Body.Close()
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && (payload.Error.Code != "" || payload.Error.Message != "") {
		return strings.TrimSpace(strings.Join([]string{payload.Error.Code, payload.Error.Message}, ": "))
	}
	return fmt.Sprintf("upstream status %d", response.StatusCode)
}

var adminPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>OpenAI Proxy Admin</title>
<style>body{font:14px system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#f5f7fb;color:#172033;margin:0;padding:24px}main{max-width:1180px;margin:auto}.card{background:#fff;border:1px solid #dfe5ef;border-radius:12px;padding:20px;margin-bottom:16px;box-shadow:0 2px 8px #1720330d}h1,h2{margin-top:0}.center{max-width:420px;margin:10vh auto}.field{display:grid;gap:8px;margin:14px 0}input{border:1px solid #cbd5e1;border-radius:7px;padding:10px;font:inherit}button{border:0;border-radius:7px;padding:8px 12px;cursor:pointer;background:#2563eb;color:#fff}button.secondary{background:#64748b}button:disabled{opacity:.55;cursor:wait}.toolbar{display:flex;gap:8px;align-items:center;flex-wrap:wrap}.muted{color:#667085}.error{color:#b42318}.ok{color:#067647}.hidden{display:none}table{border-collapse:collapse;width:100%;margin-top:14px}th,td{border-bottom:1px solid #edf0f5;padding:10px;text-align:left;vertical-align:top}pre{white-space:pre-wrap;word-break:break-word;margin:0}.test{display:flex;gap:6px;flex-wrap:wrap}.test input{width:180px;padding:6px}.result{margin-top:6px;max-width:360px}</style></head>
<body><main>
<section id="login" class="card center"><h1>OpenAI Proxy</h1><p class="muted">Sign in to manage upstream keys and view error logs.</p><form onsubmit="login(event)"><div class="field"><label for="apiKey">Admin API key</label><input id="apiKey" type="password" autocomplete="current-password" required autofocus></div><button id="loginButton" type="submit">Sign in</button><p id="loginError" class="error"></p></form></section>
<div id="app" class="hidden"><section class="card"><div class="toolbar"><h1 style="margin:0;flex:1">OpenAI Proxy Admin</h1><button class="secondary" onclick="logout()">Sign out</button></div></section><section class="card"><div class="toolbar"><h2 style="margin:0;flex:1">Upstream keys</h2><button onclick="load()">Refresh</button><span id="count" class="muted"></span></div><table><thead><tr><th>ID</th><th>Client key</th><th>Upstream key</th><th>Status</th><th>Reason</th><th>Actions</th></tr></thead><tbody id="keys"></tbody></table></section><section class="card"><div class="toolbar"><h2 style="margin:0">Test a model</h2><span class="muted">Uses GET /v1/models/{model}; no chat completion is created.</span></div><p class="muted">Each key row has its own model test field. Try a model such as <code>gpt-4o-mini</code>.</p></section><section class="card"><div class="toolbar"><h2 style="margin:0;flex:1">Recent errors</h2><span class="muted">Latest 200 errors kept in memory</span></div><pre id="logs" class="muted">Loading...</pre></section></div></main>
<script>const base=window.location.pathname.replace(/\/$/,'');const $=id=>document.getElementById(id);async function api(path,opt={}){opt.credentials='same-origin';opt.headers=Object.assign({'Content-Type':'application/json'},opt.headers||{});const r=await fetch(base+path,opt);let data=null;try{data=await r.json()}catch(e){}if(!r.ok)throw new Error((r.status===401?'401 ':'')+(data&&data.error&&data.error.message||'Request failed ('+r.status+')'));return data}async function login(event){event.preventDefault();$('loginError').textContent='';$('loginButton').disabled=true;try{await api('/api/login',{method:'POST',body:JSON.stringify({api_key:$('apiKey').value})});$('apiKey').value='';$('login').classList.add('hidden');$('app').classList.remove('hidden');await load()}catch(e){$('loginError').textContent=e.message}finally{$('loginButton').disabled=false}}async function logout(){try{await api('/api/logout',{method:'POST'})}finally{$('app').classList.add('hidden');$('login').classList.remove('hidden');$('apiKey').focus()}}async function load(){try{const s=await api('/api/status');$('count').textContent=s.error_count+' errors';$('keys').innerHTML=s.keys.map(k=>'<tr><td>'+k.id+'</td><td>'+k.client_key+'</td><td>'+k.upstream_key+'</td><td class="'+(k.enabled?'ok':'error')+'">'+(k.enabled?'Enabled':'Disabled')+'</td><td>'+esc(k.disable_cause||'')+'</td><td><div class="test"><input id="model-'+k.id+'" value="gpt-4o-mini" aria-label="Model for key '+k.id+'"><button onclick="testKey('+k.id+',this)">Test model</button><button class="secondary" onclick="action('+k.id+',\''+(k.enabled?'disable':'enable')+'\')">'+(k.enabled?'Disable':'Enable')+'</button></div><div id="result-'+k.id+'" class="result muted"></div></td></tr>').join('')||'<tr><td colspan="6">No keys configured</td></tr>';const logs=await api('/api/logs');$('logs').textContent=logs.map(x=>'['+x.at+'] '+(x.status||'')+' '+(x.method||'')+' '+(x.path||'')+' '+x.message).join('\n')||'No errors'}catch(e){if(String(e.message).includes('401')){logout();return}$('logs').textContent=e.message;$('logs').className='error'}}async function action(id,verb){await api('/api/keys/'+id+'/'+verb,{method:'POST'});await load()}async function testKey(id,button){const model=$('model-'+id).value.trim();$('result-'+id).className='result muted';$('result-'+id).textContent='Testing...';button.disabled=true;try{const r=await api('/api/keys/'+id+'/test',{method:'POST',body:JSON.stringify({model:model})});$('result-'+id).className='result '+(r.ok?'ok':'error');$('result-'+id).textContent='HTTP '+r.status+' '+(r.ok?'OK':'Failed')+(r.response?' — '+r.response:'')}catch(e){$('result-'+id).className='result error';$('result-'+id).textContent=e.message}finally{button.disabled=false}}function esc(value){return value.replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}api('/api/status').then(()=>{$('login').classList.add('hidden');$('app').classList.remove('hidden');load()}).catch(()=>{})</script></body></html>`
