package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type login struct {
	UUID, Verifier string
	Expires        time.Time
	Polling        bool
}

type credentials struct {
	Type               string    `json:"type"`
	AccessToken        string    `json:"access_token"`
	RefreshToken       string    `json:"refresh_token"`
	AccountID          string    `json:"account_id"`
	Email              string    `json:"email,omitempty"`
	Expires            time.Time `json:"expires_at"`
	DisabledModels     []string  `json:"disabled_models,omitempty"`
	ToolLoopGuardTools []string  `json:"tool_loop_guard_tools,omitempty"`
}

func randomID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic("system cryptographic randomness unavailable")
	}
	data[6] = (data[6] & 15) | 64
	data[8] = (data[8] & 63) | 128
	v := hex.EncodeToString(data[:])
	return v[:8] + "-" + v[8:12] + "-" + v[12:16] + "-" + v[16:20] + "-" + v[20:]
}

func (s *Service) startLogin() (any, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return nil, reject(500, "cannot generate login challenge")
	}
	verifier := base64.RawURLEncoding.EncodeToString(entropy[:])
	challenge := sha256.Sum256([]byte(verifier))
	state, uuid := randomID(), randomID()
	expires := time.Now().UTC().Add(10 * time.Minute)
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.logins {
		if time.Now().After(entry.Expires) && !entry.Polling {
			delete(s.logins, key)
		}
	}
	if len(s.logins) >= 16 {
		return nil, reject(409, "too many pending Cursor logins")
	}
	s.logins[state] = login{UUID: uuid, Verifier: verifier, Expires: expires}
	query := url.Values{"challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "uuid": {uuid}, "mode": {"login"}, "redirectTarget": {"cli"}}
	return map[string]any{"Provider": ID, "URL": "https://cursor.com/loginDeepControl?" + query.Encode(), "State": state, "ExpiresAt": expires}, nil
}

func (s *Service) pollLogin(ctx context.Context, raw []byte) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var request struct{ State string }
	if json.Unmarshal(raw, &request) != nil {
		return nil, reject(400, "invalid login poll")
	}
	s.mu.Lock()
	entry, exists := s.logins[request.State]
	if !exists || time.Now().After(entry.Expires) || entry.Polling {
		s.mu.Unlock()
		return nil, reject(400, "login state is expired, unknown, or already being polled")
	}
	entry.Polling = true
	s.logins[request.State] = entry
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if latest, exists := s.logins[request.State]; exists {
			latest.Polling = false
			s.logins[request.State] = latest
		}
		s.mu.Unlock()
	}()
	query := url.Values{"uuid": {entry.UUID}, "verifier": {entry.Verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/auth/poll?"+query.Encode(), nil)
	if err != nil {
		return nil, reject(500, "cannot construct login request")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, reject(502, "Cursor login could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return map[string]string{"Status": "pending"}, nil
	}
	credential, err := readTokens(resp, "")
	if err != nil {
		return nil, err
	}
	auth, err := credential.auth(true)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.logins, request.State)
	s.mu.Unlock()
	return map[string]any{"Status": "success", "Auth": auth}, nil
}

func parseCredentials(raw []byte) (credentials, error) {
	var c credentials
	if json.Unmarshal(raw, &c) != nil || c.Type != ID || c.AccessToken == "" || c.RefreshToken == "" || c.AccountID == "" {
		return c, reject(401, "Cursor credentials are missing or invalid")
	}
	return c, nil
}

func (s *Service) parseAuth(raw []byte) (any, error) {
	var request struct {
		FileName string
		RawJSON  []byte
	}
	if json.Unmarshal(raw, &request) != nil {
		return nil, reject(400, "invalid credential envelope")
	}
	c, err := parseCredentials(request.RawJSON)
	if err != nil {
		return map[string]bool{"Handled": false}, nil
	}
	auth, err := c.auth(false)
	if err != nil {
		return nil, err
	}
	auth["FileName"] = request.FileName
	return map[string]any{"Handled": true, "Auth": auth}, nil
}

func (c credentials) auth(newRecord bool) (map[string]any, error) {
	body, err := json.Marshal(c)
	if err != nil {
		return nil, reject(500, "cannot encode Cursor credentials")
	}
	label := c.Email
	if label == "" {
		label = "Cursor"
	}
	auth := map[string]any{"Provider": ID, "Label": label, "StorageJSON": body, "NextRefreshAfter": c.Expires,
		"Metadata": map[string]any{"type": ID, "email": c.Email}, "Attributes": map[string]string{"account_id": c.AccountID}}
	if newRecord {
		digest := sha256.Sum256([]byte(c.AccountID))
		name := ID + "-" + hex.EncodeToString(digest[:12]) + ".json"
		auth["ID"], auth["FileName"] = name, name
	}
	return auth, nil
}

func readTokens(resp *http.Response, fallback string) (credentials, error) {
	if resp.StatusCode != 200 {
		return credentials{}, statusError(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPayloadBytes+1))
	if err != nil || len(body) > maxPayloadBytes {
		return credentials{}, reject(502, "invalid Cursor token response")
	}
	var tokens struct{ AccessToken, RefreshToken string }
	if json.Unmarshal(body, &tokens) != nil || tokens.AccessToken == "" {
		return credentials{}, reject(502, "Cursor returned no access token")
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = fallback
	}
	parts := strings.Split(tokens.AccessToken, ".")
	if len(parts) != 3 || tokens.RefreshToken == "" {
		return credentials{}, reject(502, "Cursor returned invalid credentials")
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Sub   string
		Email string
		Exp   int64
	}
	// Claims are metadata only, never used to authorize proxy access.
	if err != nil || json.Unmarshal(claimsJSON, &claims) != nil || claims.Sub == "" || claims.Exp <= time.Now().Unix()+60 {
		return credentials{}, reject(502, "Cursor token has no valid identity or expiry")
	}
	return credentials{Type: ID, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		AccountID: claims.Sub, Email: claims.Email, Expires: time.Unix(claims.Exp, 0).UTC().Add(-time.Minute)}, nil
}

func (s *Service) refresh(ctx context.Context, raw []byte) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var request struct{ StorageJSON []byte }
	if json.Unmarshal(raw, &request) != nil {
		return nil, reject(400, "invalid refresh envelope")
	}
	c, err := parseCredentials(request.StorageJSON)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/auth/exchange_user_api_key", bytes.NewBufferString("{}"))
	if err != nil {
		return nil, reject(500, "cannot construct refresh request")
	}
	req.Header.Set("Authorization", "Bearer "+c.RefreshToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, reject(502, "Cursor token refresh could not be reached")
	}
	defer resp.Body.Close()
	old := c
	c, err = readTokens(resp, c.RefreshToken)
	if err != nil {
		return nil, err
	}
	c.DisabledModels, c.ToolLoopGuardTools = old.DisabledModels, old.ToolLoopGuardTools
	auth, err := c.auth(false)
	return map[string]any{"Auth": auth, "NextRefreshAfter": c.Expires}, err
}

func statusError(status int) error {
	switch status {
	case 401, 403:
		return reject(status, "Cursor rejected this account; sign in again or check subscription access")
	case 429:
		return reject(429, "Cursor account rate or usage limit reached")
	default:
		return reject(502, "Cursor upstream rejected the request")
	}
}
