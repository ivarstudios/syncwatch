package config

// Tests of the pre-publication fixes of the Settings area (pp-settings).

import "testing"

// import-3: syncwatch makes the API token of a file (api.token) an API token
// with the scope full. A placeholder, a cut paste or an example value is
// refused: the value has at least 10 characters and no white space, control
// or format characters. White space at the ends (a paste) goes.
func TestImportedAPITokenIsChecked(t *testing.T) {
	const want = "API token (api.token): the value must have at least 10 characters and no spaces or control characters. Make the API token in Settings → API."
	for _, tok := range []string{`x`, `changeme`, `swt_12345`, `"my token value"`, `"tab\tin-the-value"`, `"control\x01value"`,
		`"zero​width-value"`, `"no-break space"`, `"line\nbreak-value"`} {
		_, err := ParseYAML([]byte("api: {token: " + tok + "}\n"))
		if err == nil || err.Error() != want {
			t.Errorf("api.token %s: %v, want %q", tok, err, want)
		}
	}
	for tok, stored := range map[string]string{
		`swt_sekrit`:                      "swt_sekrit", // 10 characters
		`ååååååååéé`:                      "ååååååååéé", // 10 characters, 20 bytes
		`swt_kit_s7_syncwatch_0123456789`: "swt_kit_s7_syncwatch_0123456789",
		`" swt_pasted_value\n"`:           "swt_pasted_value",
		`""`:                              "",
	} {
		c, err := ParseYAML([]byte("api: {token: " + tok + "}\n"))
		if err != nil {
			t.Errorf("api.token %s: %v", tok, err)
		} else if c.API.Token != stored {
			t.Errorf("api.token %s: stored %q, want %q", tok, c.API.Token, stored)
		}
	}
}
