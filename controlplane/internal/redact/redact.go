// Package redact removes credentials from URIs and from free text that may contain them (ADR-0015).
// It imports nothing internal, so every package can use it: the service for exports and timeline
// reasons, the controller for its log, and the node agent for driver errors.
package redact

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const redacted = "REDACTED" // brackets would be percent-encoded in a URI; keep the marker readable

var (
	// uriScheme recognizes values that are URIs; anything else is left alone.
	uriScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)
	// plainHost is a host that can't hide credentials: unreserved characters and an optional port.
	plainHost = regexp.MustCompile(`^[A-Za-z0-9._~-]*(:[0-9]{1,5})?$`)
	// secretParam matches parameter names that carry credentials, such as access tokens and
	// presigned-URL signatures (X-Amz-Signature, X-Amz-Credential, and Azure SAS's sig).
	secretParam = regexp.MustCompile(`(?i)token|secret|passw|pwd|signature|credential|auth|session|key|^sig$`)
)

// URI replaces credentials in a URI with REDACTED and says why for each one. It edits the
// raw text, so everything it keeps stays byte for byte. A part it can't read reliably is removed
// too: when in doubt, redact (ADR-0015). A value that isn't a URI is returned unchanged.
func URI(value string) (string, []string) {
	scheme, rest, ok := strings.Cut(value, "://")
	if !ok || !uriScheme.MatchString(scheme) {
		return value, nil
	}
	var reasons []string
	// Credentials are everything before the last @. An unescaped password can contain /, ?, or #,
	// so when the authority looks like it holds credentials (it has an @ or a colon), the cut is
	// at the last @ anywhere in the value, not just in the authority.
	userinfo := ""
	authorityEnd := strings.IndexAny(rest, "/?#")
	if authorityEnd < 0 {
		authorityEnd = len(rest)
	}
	if strings.ContainsAny(rest[:authorityEnd], "@:") {
		if i := strings.LastIndexByte(rest, '@'); i >= 0 {
			userinfo, rest = redacted+"@", rest[i+1:]
			reasons = append(reasons, `the URI's credentials were removed (everything between "://" and the last "@")`)
		}
	}
	rest, fragment, hasFragment := strings.Cut(rest, "#")
	rest, query, hasQuery := strings.Cut(rest, "?")
	host, path := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host, path = rest[:i], rest[i:]
	}
	if !plainHost.MatchString(host) {
		host = redacted
		reasons = append(reasons, "the URI's host couldn't be read reliably, so it was removed in case it holds credentials")
	}
	if hasQuery {
		var more []string
		query, more = redactPairs(query, "query parameter")
		reasons = append(reasons, more...)
	}
	if hasFragment {
		var more []string
		fragment, more = redactPairs(fragment, "fragment parameter")
		reasons = append(reasons, more...)
	}
	if len(reasons) == 0 {
		return value, nil
	}
	var b strings.Builder
	b.WriteString(scheme + "://" + userinfo + host + path)
	if hasQuery {
		b.WriteString("?" + query)
	}
	if hasFragment {
		b.WriteString("#" + fragment)
	}
	return b.String(), reasons
}

// redactPairs removes the values of credential-like name=value pairs separated by & or ;. Every
// other byte, including the separators and the order, is kept.
func redactPairs(raw, kind string) (string, []string) {
	var b strings.Builder
	var reasons []string
	for raw != "" {
		pair, sep := raw, ""
		if i := strings.IndexAny(raw, "&;"); i >= 0 {
			pair, sep, raw = raw[:i], raw[i:i+1], raw[i+1:]
		} else {
			raw = ""
		}
		if name, value, ok := strings.Cut(pair, "="); ok && value != "" {
			switch decoded, err := url.QueryUnescape(name); {
			case err != nil:
				pair = name + "=" + redacted
				reasons = append(reasons, fmt.Sprintf("the value of %s %q was removed: its name couldn't be read, so it may be a credential", kind, name))
			case secretParam.MatchString(decoded):
				pair = name + "=" + redacted
				reasons = append(reasons, fmt.Sprintf("the value of %s %q was removed: it may be a credential", kind, decoded))
			}
		}
		b.WriteString(pair + sep)
	}
	return b.String(), reasons
}

// uriInText finds URIs inside free text such as error messages. A URI may contain ', so a match
// runs to whitespace or a character URIs never contain; trailing punctuation is redacted with it.
var uriInText = regexp.MustCompile("[A-Za-z][A-Za-z0-9+.-]*://[^\\s\"<>`]+")

// Secrets removes credentials from every URI inside free text. Use it on anything that may echo
// user input before it reaches a log line or a timeline reason.
func Secrets(text string) string {
	clean, _ := CountSecrets(text)
	return clean
}

// CountSecrets is Secrets, also saying how many values it removed, so a record of the text can
// list them (ADR-0015: never silently).
func CountSecrets(text string) (string, int) {
	n := 0
	clean := uriInText.ReplaceAllStringFunc(text, func(u string) string {
		clean, reasons := URI(u)
		n += len(reasons)
		return clean
	})
	return clean, n
}
