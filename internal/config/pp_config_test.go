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
		`"zero\u200bwidth-value"`, `"no-break\u00a0space"`, `"line\nbreak-value"`} {
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

// import-4: one site has one spelling. Each run of white space is one space
// (a no-break space from a paste looks the same as a space), and a site
// that is the same as an earlier one without regard to case gets the
// spelling of the earlier one: the site of syncwatch first, then the sites
// of the servers in order. Invisible characters are refused.
func TestOneSiteHasOneSpelling(t *testing.T) {
	// The YAML escapes \u00a0, \u2028 and \u3000.
	c, err := ParseYAML([]byte(`site: "IVAR\u00a0Office"
servers:
  - {name: AKKA, url: 'https://10.0.0.1:8384', site: "IVAR  office"}
  - {name: SOL, url: 'https://10.0.0.2:8384', site: "Martin\u2028office"}
  - {name: SIRIUS, url: 'https://10.0.0.3:8384', site: "MARTIN\u3000OFFICE "}
  - {name: VEGA, url: 'https://10.0.0.4:8384', site: "\u00a0"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Site != "IVAR Office" || c.SiteOf("akka") != "IVAR Office" || c.SiteOf("sol") != "Branch office" || c.SiteOf("sirius") != "Branch office" || c.SiteOf("vega") != "" {
		t.Fatalf("sites: %+q %+q %+q %+q %+q", c.Site, c.SiteOf("akka"), c.SiteOf("sol"), c.SiteOf("sirius"), c.SiteOf("vega"))
	}
	for _, site := range []string{`"\u3164"`, `"IVAR\u3164Office"`, `"IVAR\u200bOffice"`, `"IVAR\aOffice"`, `"IVAR\u202eOffice"`} {
		_, err := ParseYAML([]byte("site: " + site + "\n"))
		if want := "Site where syncwatch runs (site): the value must have at most 40 characters and no control characters."; err == nil || err.Error() != want {
			t.Errorf("site %s: %v", site, err)
		}
	}
	if got := CleanSite(" \tIVAR\u00a0\u00a0Office\u2029"); got != "IVAR Office" {
		t.Errorf("CleanSite: %+q", got)
	}
	// The spelling that the administrator types is the spelling of the site
	// everywhere.
	c.SpellSite("ivar OFFICE")
	if c.Site != "ivar OFFICE" || c.SiteOf("akka") != "ivar OFFICE" || c.SiteOf("sol") != "Branch office" {
		t.Fatalf("SpellSite: %+q %+q %+q", c.Site, c.SiteOf("akka"), c.SiteOf("sol"))
	}
}
