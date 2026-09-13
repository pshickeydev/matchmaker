// Package crushtest is the shared fake Crush server backing the
// milestone test suites (plan §4): an httptest server implementing the
// pinned endpoint subset of Crush v0.94.1, scriptable to emit permission
// requests, questions, completions, crashes, and stream drops, and to
// play MCP client mid-run for note-exchange scenarios. It imports
// stdlib only, so any package's tests — including crushapi's own — can
// import it without cycles.
package crushtest

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Server is one fake Crush server process.
type Server struct {
	ts *httptest.Server

	mu                sync.Mutex
	version           VersionInfo
	workspaces        map[string]*Workspace // by workspace ID
	workspaceByPath   map[string]string     // canonical path -> workspace ID
	retiredClients    map[string]bool
	subscribers       map[string][]chan []byte // workspace ID -> subscriber buffers
	submits           []Submit
	grants            []Grant
	cancels           []SessionCancel
	questionsCanceled []string
	controls          []string
	clientDeletes     []string
	workspaceDeletes  []string
	sessions          map[string]*Session // by session ID
	messages          map[string][]MessageRow
	onSubmit          func(s *Server, sub Submit)
}

// VersionInfo mirrors the GET /v1/version payload.
type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildID   string `json:"build_id"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Workspace is one fake workspace record. The response embeds a fake
// effective config with a provider key, mirroring the real secret-bearing
// payload (DESIGN §9.6).
type Workspace struct {
	ID       string            `json:"id"`
	Path     string            `json:"path"`
	YOLO     bool              `json:"yolo,omitempty"`
	Debug    bool              `json:"debug,omitempty"`
	DataDir  string            `json:"data_dir,omitempty"`
	ClientID string            `json:"client_id,omitempty"`
	Env      []string          `json:"env,omitempty"`
	Config   map[string]string `json:"config"`
}

// Submit records one accepted agent prompt.
type Submit struct {
	WorkspaceID string
	SessionID   string
	RunID       string
	Prompt      string
}

// Grant records one permission response.
type Grant struct {
	WorkspaceID string
	SessionID   string
	RequestID   string
	Action      string
}

// SessionCancel records one session cancel call.
type SessionCancel struct {
	WorkspaceID string
	SessionID   string
}

// Session is one fake dedicated session.
type Session struct {
	ID          string
	WorkspaceID string
	Busy        bool
	Title       string
}

// MessageRow is one persisted session message.
type MessageRow struct {
	ID        string
	Role      string
	SessionID string
	Parts     []Part
}

// Part is one message content part. The wire shape mirrors the real
// v0.94.1 server: {"type":"text","data":{"text":"..."}} (verified
// against a live v0.94.1 server).
type Part struct {
	Type string `json:"type"`
	Data struct {
		Text string `json:"text"`
	} `json:"data"`
}

// defaultVersion is the pinned Crush version (DESIGN §9.3).
var defaultVersion = VersionInfo{
	Version:   "v0.94.1",
	Commit:    "19e7467e",
	BuildID:   "release-19e7467e",
	GoVersion: "go1.26.8",
	Platform:  "linux/amd64",
}

// New starts one fake Crush server on an ephemeral loopback port.
func New() *Server {
	s := &Server{
		version:         defaultVersion,
		workspaces:      make(map[string]*Workspace),
		workspaceByPath: make(map[string]string),
		retiredClients:  make(map[string]bool),
		subscribers:     make(map[string][]chan []byte),
		sessions:        make(map[string]*Session),
		messages:        make(map[string][]MessageRow),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	mux.HandleFunc("POST /v1/control", s.handleControl)
	mux.HandleFunc("DELETE /v1/clients/{client_id}", s.handleDeleteClient)
	mux.HandleFunc("POST /v1/workspaces", s.handleCreateWorkspace)
	mux.HandleFunc("GET /v1/workspaces/{id}", s.handleGetWorkspace)
	mux.HandleFunc("DELETE /v1/workspaces/{id}", s.handleDeleteWorkspace)
	mux.HandleFunc("POST /v1/workspaces/{id}/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /v1/workspaces/{id}/sessions/{sid}", s.handleGetSession)
	mux.HandleFunc("GET /v1/workspaces/{id}/sessions/{sid}/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/workspaces/{id}/agent", s.handleAgent)
	mux.HandleFunc("POST /v1/workspaces/{id}/agent/sessions/{sid}/cancel", s.handleCancelSession)
	mux.HandleFunc("POST /v1/workspaces/{id}/permissions/grant", s.handleGrant)
	mux.HandleFunc("POST /v1/workspaces/{id}/questions/cancel", s.handleQuestionCancel)
	mux.HandleFunc("GET /v1/workspaces/{id}/events", s.handleEvents)
	s.ts = httptest.NewServer(mux)
	return s
}

// BaseURL returns the server's base URL.
func (s *Server) BaseURL() string { return s.ts.URL }

// Close shuts the fake server down (crash simulation).
// Close shuts the fake server down (crash simulation). Open client
// connections are force-closed first so streams held by un-consumed
// test clients cannot block teardown.
func (s *Server) Close() {
	s.ts.CloseClientConnections()
	s.ts.Close()
}

// SetVersion overrides the reported version and build_id (C4 tests).
func (s *Server) SetVersion(version, buildID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version.Version = version
	s.version.BuildID = buildID
}

// SetOnSubmit registers a script hook invoked synchronously on each
// accepted prompt submission.
func (s *Server) SetOnSubmit(hook func(s *Server, sub Submit)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onSubmit = hook
}

// SetBusy toggles a session's busy flag (recovery tests).
func (s *Server) SetBusy(sessionID string, busy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[sessionID]; ok {
		session.Busy = busy
	}
}

// AddMessage appends one session message (recovery proof tests).
func (s *Server) AddMessage(sessionID, role, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	part := Part{Type: "text"}
	part.Data.Text = text
	s.messages[sessionID] = append(s.messages[sessionID], MessageRow{
		ID:        newUUID(),
		Role:      role,
		SessionID: sessionID,
		Parts:     []Part{part},
	})
}

// WorkspaceByPath returns the workspace serving the canonical path.
func (s *Server) WorkspaceByPath(path string) (*Workspace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.workspaceByPath[path]
	if !ok {
		return nil, false
	}
	return s.workspaces[id], true
}

// Workspace returns the workspace by ID.
func (s *Server) Workspace(id string) (*Workspace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	workspace, ok := s.workspaces[id]
	return workspace, ok
}

// Session returns a session by ID.
func (s *Server) Session(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	return session, ok
}

// Submits returns every accepted prompt in order.
func (s *Server) Submits() []Submit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Submit(nil), s.submits...)
}

// Grants returns every permission response in order.
func (s *Server) Grants() []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Grant(nil), s.grants...)
}

// Cancels returns every session cancel call in order.
func (s *Server) Cancels() []SessionCancel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SessionCancel(nil), s.cancels...)
}

// QuestionsCanceled returns the workspaces whose question batches were
// canceled.
func (s *Server) QuestionsCanceled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.questionsCanceled...)
}

// Controls returns every control command issued.
func (s *Server) Controls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.controls...)
}

// WorkspaceDeletes returns every workspace hold release.
func (s *Server) WorkspaceDeletes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.workspaceDeletes...)
}

// ClientDeletes returns every retired client identity.
func (s *Server) ClientDeletes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.clientDeletes...)
}

// SubscriberCount reports open SSE subscribers of a workspace, so
// callers can wait for an attach before emitting.
func (s *Server) SubscriberCount(workspaceID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subscribers[workspaceID])
}

// Emit publishes one SSE event payload on a workspace's stream. The wire
// shape mirrors the real v0.94.1 server: a {type, payload} envelope whose
// payload is itself {type, payload} holding the typed event data (verified
// against a live v0.94.1 server).
func (s *Server) Emit(workspaceID, kind string, payload any) error {
	encoded, err := json.Marshal(struct {
		Type    string `json:"type"`
		Payload any    `json:"payload"`
	}{Type: kind, Payload: struct {
		Type    string `json:"type"`
		Payload any    `json:"payload"`
	}{Type: "updated", Payload: payload}})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, subscriber := range s.subscribers[workspaceID] {
		subscriber <- encoded
	}
	return nil
}

// EmitPermission publishes a permission_request on a workspace.
func (s *Server) EmitPermission(workspaceID string, request map[string]any) error {
	return s.Emit(workspaceID, "permission_request", request)
}

// CompleteRun publishes a run_complete event echoing the RunID.
func (s *Server) CompleteRun(workspaceID, sessionID, runID, text, runError string, cancelled bool) error {
	return s.Emit(workspaceID, "run_complete", map[string]any{
		"session_id": sessionID,
		"run_id":     runID,
		"message_id": newUUID(),
		"text":       text,
		"error":      runError,
		"cancelled":  cancelled,
	})
}

// DropStream simulates stream loss for every open SSE connection of a
// workspace (C1 teardown tests).
func (s *Server) DropStream(workspaceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.DropStreamLocked(workspaceID)
}

// RemoveWorkspace tears a workspace down as Crush's claim lifecycle does
// (C1): later gets return 404 and open streams close.
func (s *Server) RemoveWorkspace(workspaceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if workspace, ok := s.workspaces[workspaceID]; ok {
		delete(s.workspaceByPath, workspace.Path)
	}
	delete(s.workspaces, workspaceID)
	s.DropStreamLocked(workspaceID)
}

// DropStreamLocked closes subscribers; the caller holds s.mu.
func (s *Server) DropStreamLocked(workspaceID string) {
	for _, subscriber := range s.subscribers[workspaceID] {
		close(subscriber)
	}
	s.subscribers[workspaceID] = nil
}

// RetireClient refuses later workspace creates from the client (C1).
func (s *Server) RetireClient(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retiredClients[clientID] = true
}

// handleHealth serves GET /v1/health.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleVersion serves GET /v1/version.
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	version := s.version
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, version)
}

// handleControl serves POST /v1/control: shutdown and shutdown_if_idle
// are idle-guarded while workspaces live (verified v0.94.1).
func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "failed to decode request")
		return
	}
	s.mu.Lock()
	s.controls = append(s.controls, body.Command)
	live := len(s.workspaces)
	s.mu.Unlock()
	if live > 0 {
		writeError(w, http.StatusConflict, "workspaces are live")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{})
}

// handleDeleteClient serves DELETE /v1/clients/{client_id}.
func (s *Server) handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("client_id")
	s.mu.Lock()
	s.clientDeletes = append(s.clientDeletes, clientID)
	s.retiredClients[clientID] = true
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{})
}

// handleCreateWorkspace serves POST /v1/workspaces: duplicates dedupe by
// canonical path, first-create-wins (verified v0.94.1).
func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID string   `json:"client_id"`
		Path     string   `json:"path"`
		YOLO     bool     `json:"yolo"`
		Debug    bool     `json:"debug"`
		DataDir  string   `json:"data_dir"`
		Env      []string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "failed to decode request")
		return
	}
	if !isUUID(body.ClientID) {
		writeError(w, http.StatusBadRequest, "client_id is not a valid UUID")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retiredClients[body.ClientID] {
		writeError(w, http.StatusForbidden, "client is retired")
		return
	}
	if id, ok := s.workspaceByPath[body.Path]; ok {
		writeJSON(w, http.StatusOK, s.workspaces[id])
		return
	}
	workspace := &Workspace{
		ID:       newUUID(),
		Path:     body.Path,
		YOLO:     body.YOLO,
		Debug:    body.Debug,
		DataDir:  body.DataDir,
		ClientID: body.ClientID,
		Env:      body.Env,
		Config:   map[string]string{"provider.api_key": "fake-secret-key"},
	}
	s.workspaces[workspace.ID] = workspace
	s.workspaceByPath[body.Path] = workspace.ID
	writeJSON(w, http.StatusOK, workspace)
}

// handleGetWorkspace serves GET /v1/workspaces/{id}.
func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	workspace, ok := s.workspaces[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	writeJSON(w, http.StatusOK, workspace)
}

// handleDeleteWorkspace serves DELETE /v1/workspaces/{id}?client_id=.
func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	if !isUUID(clientID) {
		writeError(w, http.StatusBadRequest, "client_id is not a valid UUID")
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaceDeletes = append(s.workspaceDeletes, id)
	if workspace, ok := s.workspaces[id]; ok {
		delete(s.workspaceByPath, workspace.Path)
		delete(s.workspaces, id)
	}
	s.DropStreamLocked(id)
	writeJSON(w, http.StatusOK, map[string]bool{})
}

// handleCreateSession serves POST /v1/workspaces/{id}/sessions.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.workspaces[r.PathValue("id")]; !ok {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	session := &Session{ID: newUUID(), WorkspaceID: r.PathValue("id")}
	s.sessions[session.ID] = session
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      session.ID,
		"is_busy": session.Busy,
		"title":   session.Title,
	})
}

// handleGetSession serves GET /v1/workspaces/{id}/sessions/{sid}.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessionFor(r.PathValue("id"), r.PathValue("sid"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      session.ID,
		"is_busy": session.Busy,
		"title":   session.Title,
	})
}

// sessionFor resolves a session only through its owning workspace, as
// workspace teardown removes its sessions (C1).
func (s *Server) sessionFor(workspaceID, sessionID string) (*Session, bool) {
	if _, ok := s.workspaces[workspaceID]; !ok {
		return nil, false
	}
	session, ok := s.sessions[sessionID]
	if !ok || session.WorkspaceID != workspaceID {
		return nil, false
	}
	return session, true
}

// handleMessages serves GET /v1/workspaces/{id}/sessions/{sid}/messages.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessionFor(r.PathValue("id"), r.PathValue("sid")); !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	rows := s.messages[r.PathValue("sid")]
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":         row.ID,
			"role":       row.Role,
			"session_id": row.SessionID,
			"parts":      row.Parts,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAgent serves POST /v1/workspaces/{id}/agent: fire-and-forget,
// 202 with an empty body (verified v0.94.1).
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		RunID     string `json:"run_id"`
		Prompt    string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "failed to decode request")
		return
	}
	s.mu.Lock()
	workspaceID := r.PathValue("id")
	if _, ok := s.workspaces[workspaceID]; !ok {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if _, ok := s.sessions[body.SessionID]; !ok {
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, "session not found")
		return
	}
	sub := Submit{WorkspaceID: workspaceID, SessionID: body.SessionID, RunID: body.RunID, Prompt: body.Prompt}
	s.submits = append(s.submits, sub)
	hook := s.onSubmit
	s.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	if hook != nil {
		hook(s, sub)
	}
}

// handleCancelSession serves POST .../agent/sessions/{sid}/cancel.
func (s *Server) handleCancelSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels = append(s.cancels, SessionCancel{
		WorkspaceID: r.PathValue("id"),
		SessionID:   r.PathValue("sid"),
	})
	writeJSON(w, http.StatusOK, map[string]bool{})
}

// handleGrant serves POST .../permissions/grant: the body echoes the
// full permission request back alongside the action (verified v0.94.1).
func (s *Server) handleGrant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Permission struct {
			ID        string          `json:"id"`
			SessionID string          `json:"session_id"`
			Params    json.RawMessage `json:"params"`
		} `json:"permission"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "failed to decode request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, Grant{
		WorkspaceID: r.PathValue("id"),
		SessionID:   body.Permission.SessionID,
		RequestID:   body.Permission.ID,
		Action:      body.Action,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"resolved": true})
}

// handleQuestionCancel serves POST .../questions/cancel.
func (s *Server) handleQuestionCancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.questionsCanceled = append(s.questionsCanceled, r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]bool{})
}

// handleEvents serves GET /v1/workspaces/{id}/events as data-only SSE
// (verified v0.94.1): every frame is `data: {json}` with no event lines.
// The client_id query parameter is required; the SSE stream is the
// workspace's client claim (C1).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	workspaceID := r.PathValue("id")
	if r.URL.Query().Get("client_id") == "" {
		writeError(w, http.StatusBadRequest, "client_id is required")
		return
	}
	s.mu.Lock()
	if _, ok := s.workspaces[workspaceID]; !ok {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	subscriber := make(chan []byte, 16)
	s.subscribers[workspaceID] = append(s.subscribers[workspaceID], subscriber)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher := w.(http.Flusher)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			s.unsubscribe(workspaceID, subscriber)
			return
		case payload, ok := <-subscriber:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

// unsubscribe removes one closed or departing SSE subscriber.
func (s *Server) unsubscribe(workspaceID string, subscriber chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.subscribers[workspaceID]
	kept := current[:0]
	for _, candidate := range current {
		if candidate != subscriber {
			kept = append(kept, candidate)
		}
	}
	s.subscribers[workspaceID] = kept
}

// writeJSON encodes one successful response.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// writeError encodes one error response.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// newUUID generates one fake UUID-shaped identifier.
func newUUID() string {
	var bytes [16]byte
	rand.Read(bytes[:])
	hex := fmt.Sprintf("%x", bytes[:])
	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32]
}

// isUUID reports whether s is UUID-shaped.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	return s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-' &&
		!strings.ContainsAny(s, "ghijklmnopqrstuvwxyzGHJKLMNOPQRSTUVWXYZ")
}
