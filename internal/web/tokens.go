package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// apiSettingsView is what the API card of the Settings page shows
// (templates/settings_api.html).
type apiSettingsView struct {
	Tokens  []apiTokenRow // oldest first
	Max     int           // store.MaxAPITokens
	MaxName int           // store.MaxTokenName
	CanAdd  bool          // fewer than Max API tokens exist
	// Old: the single API token of an older version (the secret api.token)
	// is stored and is not a row of api_tokens; syncwatch does not accept
	// it. OldLost: and syncwatch cannot decrypt it.
	Old, OldLost bool
	Error        string // why syncwatch refused the last form of the card
	Name, Scope  string // the values of the "Add API token" form
}

// apiTokenRow is one API token in the list of the API card.
type apiTokenRow struct {
	ID, Name, ScopeLabel string
	Created, LastUsed    string // in the configured time zone; LastUsed is "never" when not used
	ReplaceConfirm       string // the questions before Replace and Revoke
	RevokeConfirm        string
}

// newAPIToken is the new value of an API token, for the panel that shows it
// one time ("settings-api-new-token").
type newAPIToken struct {
	Name, Scope, Value string
}

// errTokenForm answers a form of the API card that has no known op.
var errTokenForm = errors.New("The form is not valid. Reload the page.")

// scopeLabel names the scope of an API token on the dashboard.
func scopeLabel(scope string) string {
	if scope == store.ScopeHA {
		return "Home Assistant: /api/v1/ha only"
	}
	return "Full: /api/v1/status and /api/v1/ha"
}

// scopeSentence says what an API token with the scope can read, for the
// settings notices.
func scopeSentence(scope string) string {
	if scope == store.ScopeHA {
		return "It can read /api/v1/ha only."
	}
	return "It can read /api/v1/status and /api/v1/ha."
}

// quoted puts the name of an API token in quotation marks for a text.
func quoted(name string) string { return `"` + name + `"` }

// apiSettings returns the API tokens for the Settings page.
func (s *Server) apiSettings() apiSettingsView {
	v := apiSettingsView{Max: store.MaxAPITokens, MaxName: store.MaxTokenName, Scope: store.ScopeHA}
	tokens, err := s.st.APITokens()
	if err != nil {
		s.log.Error("cannot read the API tokens", "err", err)
	}
	for _, t := range tokens {
		used := "never"
		if !t.LastUsed.IsZero() {
			used = s.fmtTime(t.LastUsed)
		}
		lose := " The programs with this API token lose access immediately."
		v.Tokens = append(v.Tokens, apiTokenRow{ID: t.ID, Name: t.Name, ScopeLabel: scopeLabel(t.Scope), Created: s.fmtTime(t.CreatedAt), LastUsed: used,
			ReplaceConfirm: "Replace the API token " + quoted(t.Name) + "?" + lose, RevokeConfirm: "Revoke the API token " + quoted(t.Name) + "?" + lose})
	}
	v.CanAdd = err == nil && len(tokens) < store.MaxAPITokens
	v.Old, v.OldLost = s.st.HasSecret(store.SecretAPIToken), s.undecryptable(store.SecretAPIToken)
	return v
}

// touchAPIToken records the use of the API token t, which the request read
// with the value token, at most once a minute for each API token: the last
// use in the row that the request read decides. Thus the time on the
// Settings page is at most 1 minute late. The record goes by the value:
// after a replace between the read and the record, the new value stays
// unused.
func (s *Server) touchAPIToken(t *store.APIToken, token string) {
	now := s.now()
	if d := now.Sub(t.LastUsed); !t.LastUsed.IsZero() && d >= 0 && d < time.Minute {
		return
	}
	if err := s.st.TouchAPIToken(token, now); err != nil && !errors.Is(err, store.ErrNoToken) {
		if n := s.logs.count("touch "+t.ID, now); n > 0 {
			s.log.Warn("cannot record the use of the API token", "token", t.Name, "err", err, "count", n)
		}
	}
}

// handleAPITokens adds, replaces and revokes the API tokens: POST
// /settings/api-tokens with op add (name, scope), replace (id), revoke (id)
// or revoke-old (the API token of an older version). The route of the
// single API token of an older version, POST /settings/api-token, works on
// the API token named "API token": op revoke revokes it, every other op
// gives it a new value, or adds it with the scope full.
func (s *Server) handleAPITokens(w http.ResponseWriter, r *http.Request, info requestInfo) {
	op := r.FormValue("op")
	if r.URL.Path == "/settings/api-token" {
		legacy, err := s.st.APITokenByName(store.LegacyTokenName)
		switch {
		case err != nil:
			s.refuseAPIToken(w, r, info, err, "", "")
		case op == "revoke" && legacy == nil && s.st.HasSecret(store.SecretAPIToken):
			s.revokeOldAPIToken(w, r, info)
		case op == "revoke" && legacy == nil:
			s.refuseAPIToken(w, r, info, store.ErrNoToken, "", "")
		case op == "revoke":
			s.revokeAPIToken(w, r, info, legacy.ID)
		case legacy != nil:
			s.replaceAPIToken(w, r, info, legacy.ID)
		default:
			s.addAPIToken(w, r, info, store.LegacyTokenName, store.ScopeFull)
		}
		return
	}
	switch op {
	case "add":
		s.addAPIToken(w, r, info, r.FormValue("name"), r.FormValue("scope"))
	case "replace":
		s.replaceAPIToken(w, r, info, r.FormValue("id"))
	case "revoke":
		s.revokeAPIToken(w, r, info, r.FormValue("id"))
	case "revoke-old":
		s.revokeOldAPIToken(w, r, info)
	default:
		s.refuseAPIToken(w, r, info, errTokenForm, "", "")
	}
}

// newTokenValue makes the value of a new API token.
func newTokenValue() string { return "swt_" + randomToken(24) }

func (s *Server) addAPIToken(w http.ResponseWriter, r *http.Request, info requestInfo, name, scope string) {
	value := newTokenValue()
	t, err := s.st.AddAPIToken(name, scope, value, s.now())
	if err != nil {
		s.refuseAPIToken(w, r, info, err, name, scope)
		return
	}
	s.announce(r, notify.Notice{Title: "API token added", Summary: "The API token " + quoted(t.Name) + " is added. " + scopeSentence(t.Scope), Security: true})
	s.showAPIToken(w, r, info, t, value)
}

func (s *Server) replaceAPIToken(w http.ResponseWriter, r *http.Request, info requestInfo, id string) {
	t, err := s.apiTokenByID(id)
	value := newTokenValue()
	if err == nil {
		err = s.st.ReplaceAPIToken(t.ID, value, s.now())
	}
	if err != nil {
		s.refuseAPIToken(w, r, info, err, "", "")
		return
	}
	s.announce(r, notify.Notice{Title: "API token replaced", Summary: "The API token " + quoted(t.Name) + " is replaced. syncwatch refuses the old value now.", Security: true})
	s.showAPIToken(w, r, info, t, value)
}

func (s *Server) revokeAPIToken(w http.ResponseWriter, r *http.Request, info requestInfo, id string) {
	t, err := s.apiTokenByID(id)
	if err == nil {
		err = s.st.DeleteAPIToken(t.ID)
	}
	if err != nil {
		s.refuseAPIToken(w, r, info, err, "", "")
		return
	}
	s.announce(r, notify.Notice{Title: "API token revoked", Summary: "The API token " + quoted(t.Name) + " is revoked. syncwatch refuses its value now.", Security: true})
	s.redirect(w, r, "/settings#api", "The API token "+quoted(t.Name)+" is revoked.", false)
}

// revokeOldAPIToken deletes the single API token of an older version that
// the store did not move into api_tokens (the secret api.token).
func (s *Server) revokeOldAPIToken(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if !s.st.HasSecret(store.SecretAPIToken) {
		s.refuseAPIToken(w, r, info, store.ErrNoToken, "", "")
		return
	}
	if err := s.st.DeleteSecret(store.SecretAPIToken); err != nil {
		s.refuseAPIToken(w, r, info, err, "", "")
		return
	}
	s.announce(r, notify.Notice{Title: "API token revoked", Summary: "The API token of an older version is revoked.", Security: true})
	s.redirect(w, r, "/settings#api", "The API token of an older version is revoked.", false)
}

// apiTokenByID returns the API token with the ID, or store.ErrNoToken.
func (s *Server) apiTokenByID(id string) (store.APIToken, error) {
	tokens, err := s.st.APITokens()
	if err != nil {
		return store.APIToken{}, err
	}
	for _, t := range tokens {
		if t.ID == id {
			return t, nil
		}
	}
	return store.APIToken{}, store.ErrNoToken
}

// showAPIToken answers with the Settings page and the new value of an API
// token at the top. syncwatch shows the value only this one time.
func (s *Server) showAPIToken(w http.ResponseWriter, r *http.Request, info requestInfo, t store.APIToken, value string) {
	s.render(w, r, http.StatusOK, "settings", info, s.settingsData(info, map[string]any{"NewToken": newAPIToken{Name: t.Name, Scope: t.Scope, Value: value}}))
}

// refuseAPIToken answers a refused form of the API card with the Settings
// page (HTTP 422) and the reason in the card; name and scope stay in the
// "Add API token" form. An error of the database is an internal error.
func (s *Server) refuseAPIToken(w http.ResponseWriter, r *http.Request, info requestInfo, err error, name, scope string) {
	known := false
	for _, e := range []error{store.ErrTokenName, store.ErrTokenScope, store.ErrTooManyTokens, store.ErrNoToken, errTokenForm} {
		known = known || errors.Is(err, e)
	}
	if !known {
		s.log.Error("cannot change the API tokens", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v := s.apiSettings()
	v.Error, v.Name = err.Error(), name
	if scope == store.ScopeFull {
		v.Scope = scope
	}
	s.render(w, r, http.StatusUnprocessableEntity, "settings", info, s.settingsData(info, map[string]any{"API": v}))
}
