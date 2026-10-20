package jsonrpc

import (
	"context"
	"net/http"
	"strings"
)

// Validator validates a raw Authorization header value -- the bearer
// token exactly as sent, with no "Bearer " prefix, matching the
// P3AuthToken convention this codebase's clients already use -- and
// returns the authenticated user id.
//
// Deliberately just this interface, with no dependency on any concrete
// implementation, so this package depends only on the shape it needs and
// every test can supply its own. The real implementation is
// BV-BRC-Go-SDK/v2/p3auth; see cmd/sra-cache-server/main.go's construction
// site for why it is not wired up yet.
type Validator interface {
	Validate(token string) (userID string, err error)
}

// AuthInfo is attached to a dispatched request's context; retrieve it
// with AuthInfoFromContext. It is present -- possibly with Authenticated
// == false -- for every dispatched call, including AuthNone ones.
type AuthInfo struct {
	UserID        string
	Token         string
	Authenticated bool
}

type contextKey int

const (
	authInfoKey contextKey = iota
	requestKey
)

func withAuthInfo(ctx context.Context, info AuthInfo) context.Context {
	return context.WithValue(ctx, authInfoKey, info)
}

// AuthInfoFromContext retrieves the AuthInfo a Handler's context carries.
// Called outside a dispatched request (e.g. in a test that invokes a
// Handler directly) it returns the zero value, which reads as
// unauthenticated -- the same state an AuthNone or AuthOptional-with-no-header
// call attaches.
func AuthInfoFromContext(ctx context.Context) AuthInfo {
	info, _ := ctx.Value(authInfoKey).(AuthInfo)
	return info
}

func withRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, requestKey, r)
}

// RequestFromContext retrieves the *http.Request a Handler is running
// within, e.g. so a handler can call ClientIP for logging. Returns nil
// outside a dispatched request.
func RequestFromContext(ctx context.Context) *http.Request {
	r, _ := ctx.Value(requestKey).(*http.Request)
	return r
}

// authenticate implements p3_server_stub.tt's call_method auth block
// (template lines ~245-276) with its exact asymmetry preserved. Reading
// the template directly (not the implementation plan's paraphrase of it)
// matters here because of a subtlety in how it's actually written:
//
//	my $method_auth = $method_authentication{$method};
//	$ctx->authenticated(0);
//	if ($method_auth eq 'none') {
//	    # No authentication required here. Move along.
//	} else {
//	    my $token = $self->_plack_req->header("Authorization");
//	    if (!$token && $method_auth eq 'required') {
//	        $self->exception('PerlError', "Authentication required for ...");
//	    }
//	    my $auth_token = P3AuthToken->new(token => $token, ignore_authrc => 1);
//	    my($valid, $validate_err) = $self->validator->validate($auth_token);
//	    if ($method_auth eq 'required' && !$valid) {
//	        $self->exception('PerlError', "Token validation failed: ...");
//	    } elsif ($valid) {
//	        $ctx->authenticated(1); ...
//	    }
//	}
//
// For AuthOptional (method_auth 'optional') the Perl code does not
// short-circuit on a missing token the way it does for 'required': it
// still constructs a P3AuthToken from an undef $token and calls
// ->validate on it. But since $method_auth ne 'required', no exception is
// ever raised regardless of $valid, and an always-undef token can never
// come back valid -- so the observable outcome (proceed, unauthenticated,
// no error) is identical whether or not that pointless Validate call
// happens. This port skips it, which is behavior-preserving and avoids a
// meaningless call into a possibly-network-backed Validator on every
// unauthenticated AuthOptional request.
//
// The one genuinely asymmetric case, verified directly against the
// `if ($method_auth eq 'required' && !$valid)` guard above -- it only
// ever fires for 'required' -- is: AuthOptional with a header present
// that fails Validate proceeds unauthenticated, with no error. A
// Required-shaped implementation (error on any invalid token) would get
// this wrong; SRACache needs it right because get_sra_data and friends
// must serve both anonymous callers and callers whose token has simply
// expired.
func authenticate(policy AuthPolicy, moduleName, authHeader string, validator Validator) (AuthInfo, *Error) {
	if policy == AuthNone {
		return AuthInfo{}, nil
	}

	if authHeader == "" {
		if policy == AuthRequired {
			return AuthInfo{}, NewAuthRequiredMissing(moduleName)
		}
		return AuthInfo{}, nil
	}

	userID, err := validator.Validate(authHeader)
	if err != nil {
		if policy == AuthRequired {
			return AuthInfo{}, NewAuthTokenInvalid(err)
		}
		return AuthInfo{}, nil
	}

	return AuthInfo{UserID: userID, Token: authHeader, Authenticated: true}, nil
}

// ClientIP derives a diagnostic client address, matching
// p3_server_stub.tt's getIPAddress: the first entry of X-Forwarded-For,
// else X-Real-IP, else the connection's own remote address.
//
// This is diagnostic/logging only and must NEVER be used for
// authorization decisions: both headers are attacker-suppliable unless a
// trusted reverse proxy strips or overwrites them before this service
// sees the request, and this package has no way to know whether that's
// true of wherever it's deployed. (Workspace/CLAUDE.md documents a real
// nginx misconfiguration of exactly this kind -- `$proxy_add_x_forwarded_for`
// instead of `$remote_addr` lets a client inject a spoofed first hop.)
func ClientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	return r.RemoteAddr
}
