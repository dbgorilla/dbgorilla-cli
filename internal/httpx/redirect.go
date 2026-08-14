// Package httpx holds the HTTP behavior the API and auth clients must agree
// on. It exists because those two packages cannot share code any other way --
// api imports auth, so auth cannot import api -- and the redirect policy is
// exactly the kind of rule that must not drift between them.
package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// CrossHostRedirectError reports that a request was redirected to a different
// host and was not followed.
//
// This is a configuration problem wearing a disguise. A deployment that has
// moved answers every path with a redirect to its new home, usually to the
// site root rather than the matching path. Following it lands on a web page
// that returns 200, so a status check passes and the CLI fails later trying to
// read JSON out of HTML -- reporting a parse error about a "<" character,
// which tells the user nothing they can act on.
type CrossHostRedirectError struct {
	From string // the URL that was requested
	To   string // where it tried to send us
}

func (e *CrossHostRedirectError) Error() string {
	return fmt.Sprintf(
		"%s redirected to a different host: %s\n"+
			"  Nothing was sent to either host beyond the original request.\n"+
			"  If that is where your deployment now lives, point the CLI at it:\n"+
			"    dbg config set api-url %s",
		e.From, e.To, e.To)
}

// RedirectPolicy is the CheckRedirect for every client in this CLI.
//
// It refuses three things: a downgrade to plain HTTP (unless the caller has
// explicitly opted out of TLS verification), an endless chain, and a redirect
// that leaves the host the request was aimed at.
//
// The cross-host rule is the one that matters in practice. Credentials and
// tokens ride on these requests, and Go's default client would resend them to
// wherever it was pointed. Refusing also turns a stale configured URL into a
// message naming the new host instead of a JSON parse error.
func RedirectPolicy(insecure bool) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !insecure && req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect to non-https URL: %s", req.URL.Redacted())
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if len(via) > 0 {
			origin := via[0].URL
			if !strings.EqualFold(req.URL.Host, origin.Host) {
				return &CrossHostRedirectError{
					From: origin.Scheme + "://" + origin.Host,
					To:   req.URL.Scheme + "://" + req.URL.Host,
				}
			}
		}
		return nil
	}
}

// IsHTML reports whether a response body is markup -- a web page -- rather
// than the JSON an API endpoint should return. Used to say so plainly instead
// of surfacing a decoder complaining about a "<" character.
//
// The test is the first character, not a search for tag names. Searching would
// misread a legitimate JSON error whose message happens to quote markup, e.g.
// {"detail":"<head> is not allowed here"}, and reporting that as "a web page"
// sends the user hunting for a proxy that does not exist. JSON begins with
// "{" or "["; markup begins with "<".
func IsHTML(body []byte) bool {
	trimmed := strings.TrimSpace(string(body))
	return strings.HasPrefix(trimmed, "<")
}
