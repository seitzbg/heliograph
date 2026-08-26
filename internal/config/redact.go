package config

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
)

// secretParamRedactors maps a probe param name that can embed credentials to the function that
// sanitizes its value for a non-admin config read (CODE_REVIEW M11). Today only the HTTP probe's
// urlformat qualifies: it can carry a credential in its userinfo (user:pass@), its query (?token=…),
// or its path (…/hooks/TOKEN), all of which the request actually uses. Add a probe's secret-capable
// param here when introduced.
var secretParamRedactors = map[string]func(string) string{
	"urlformat": redactURLFormat,
}

// RedactSecrets returns a copy of a config document (the DB fragment or the effective config, as
// JSON) with every secret-capable probe param value sanitized. The stored config is never changed —
// only what an unauthenticated reader receives. An authenticated admin is served the real document
// instead: it is the editable source, so redacting it would let the next save overwrite the secret
// with its masked form. A parse failure is returned so the caller can fail closed rather than serve
// an unredacted doc.
func RedactSecrets(doc json.RawMessage) (json.RawMessage, error) {
	if t := bytes.TrimSpace(doc); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return doc, nil
	}
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return nil, err
	}
	redactValue(v)
	return json.Marshal(v)
}

// redactValue walks a decoded JSON value, redacting the value of any map key that names a
// secret-capable probe param, wherever it appears (a target node's `params`, a probe-level default
// under `probes`, or any future location). Non-secret keys are recursed into.
func redactValue(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				if red := secretParamRedactors[k]; red != nil {
					t[k] = red(s)
					continue
				}
			}
			redactValue(val)
		}
	case []any:
		for _, e := range t {
			redactValue(e)
		}
	}
}

// redactURLFormat sanitizes a urlformat template for a non-admin config read. It strips HTTP
// userinfo (user:pass@), the query string, and the fragment, and masks a non-root path — path
// segments are a common credential carrier (Discord/Slack/PagerDuty webhook tokens live in the path,
// not the userinfo or query), and a field-name table cannot tell an informative "/health" from a
// secret "/hooks/TOKEN" (CODE_REVIEW M11). The scheme, host and port stay visible so the read-only
// view still shows what is probed (e.g. https://%host%/[redacted]), and the %host% placeholder is
// preserved. A value that can carry no credential (scheme://%host%, optionally with a port or a bare
// "/") round-trips unchanged; a value that does not parse is fully masked.
func redactURLFormat(v string) string {
	const ph = "heliograph-host-placeholder.invalid"
	u, err := url.Parse(strings.ReplaceAll(v, "%host%", ph))
	if err != nil {
		return "[redacted]"
	}
	nonRootPath := u.Path != "" && u.Path != "/"
	if u.User == nil && u.RawQuery == "" && u.Fragment == "" && !nonRootPath {
		return v // nothing that can carry a credential; keep the template byte-for-byte
	}
	// Rebuild from the safe components only (scheme://host[:port]), masking any non-root path.
	// Building the string by hand keeps the "[redacted]" marker literal (url.String would %-escape
	// the brackets).
	red := u.Scheme + "://" + u.Host
	switch {
	case nonRootPath:
		red += "/[redacted]"
	case u.Path == "/":
		red += "/"
	}
	return strings.ReplaceAll(red, ph, "%host%")
}
