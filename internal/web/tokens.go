package web

import (
	"net/http"

	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// apiSettingsView is what the API card of the Settings page shows
// (templates/settings_api.html).
type apiSettingsView struct {
	HasToken  bool // an API token works
	TokenLost bool // an API token is stored, but syncwatch cannot decrypt it
}

// apiSettings returns the state of the API token for the Settings page.
func (s *Server) apiSettings() apiSettingsView {
	return apiSettingsView{HasToken: s.hasAPIToken(), TokenLost: s.undecryptable(store.SecretAPIToken)}
}

// hasAPIToken reports whether an API token is set: the secret api.token, or
// a row of api_tokens (at the start, the store moves the secret of an older
// version into api_tokens).
func (s *Server) hasAPIToken() bool {
	if s.st.HasSecret(store.SecretAPIToken) {
		return true
	}
	tokens, err := s.st.APITokens()
	return err == nil && len(tokens) > 0
}

// dropAPITokens revokes the rows of api_tokens, so that a replaced or a
// revoked API token also stops working after the store moved it there.
func (s *Server) dropAPITokens() {
	tokens, err := s.st.APITokens()
	if err != nil {
		s.log.Error("cannot read the API tokens", "err", err)
		return
	}
	for _, t := range tokens {
		if err := s.st.DeleteAPIToken(t.ID); err != nil {
			s.log.Error("cannot revoke the API token", "err", err)
		}
	}
}

// handleAPITokens generates, replaces or revokes the API token.
func (s *Server) handleAPITokens(w http.ResponseWriter, r *http.Request, info requestInfo) {
	had := s.hasAPIToken()
	if r.FormValue("op") == "revoke" {
		_ = s.st.DeleteSecret(store.SecretAPIToken)
		s.dropAPITokens()
		if had {
			s.announce(r, notify.Notice{Title: "API token revoked", Summary: "syncwatch refuses requests with the old API token."})
		}
		s.redirect(w, r, "/settings#api", "The API token is revoked.", false)
		return
	}
	tok := "swt_" + randomToken(24)
	if err := s.st.SetSecret(store.SecretAPIToken, tok); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.dropAPITokens()
	title, summary := "API token generated", "The API token gives read access to /api/v1/status and /api/v1/ha."
	if had {
		title, summary = "API token replaced", summary+" syncwatch refuses the old API token now."
	}
	s.announce(r, notify.Notice{Title: title, Summary: summary})
	s.render(w, r, http.StatusOK, "settings", info, s.settingsData(info, map[string]any{"NewToken": tok}))
}
