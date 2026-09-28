package jsonrpc

import (
	"fmt"
	"net/http"
)

// Ping implements /ping: an unauthenticated liveness check, byte for byte
// from p3_server_stub.tt's `ping` sub:
//
//	sub ping {
//	    my($self, $env) = @_;
//	    return [ 200, ["Content-type" => "text/plain"], [ "OK\n" ] ];
//	}
//
// i.e. HTTP 200, Content-Type: text/plain, body "OK\n", regardless of
// auth state -- there is no auth check in the Perl sub at all.
//
// This is identical, function-for-function, to Phase 1's original
// /ping handler in cmd/sra-cache-server/main.go (also named handlePing
// there); Phase 2 makes this the one implementation Server.ServeHTTP
// actually serves from, and cmd/sra-cache-server/main.go's handlePing now
// just calls this so its pre-existing tests keep passing unchanged.
//
// The GET/HEAD-only + 405-otherwise behavior below is stricter than the
// Perl sub (which, mounted via Plack::Builder's `mount "/ping"`, responds
// the same way to any HTTP method) -- that restriction predates this
// package (Phase 1) and is preserved for backward compatibility with its
// existing test, not because the real service enforces it.
func Ping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		fmt.Fprint(w, "OK\n")
	}
}

// handleAuthPing implements /auth_ping: an authenticated liveness check.
// Byte for byte from p3_server_stub.tt's `auth_ping` sub (authenticated
// branch -- SRACache declares AuthRequired/AuthOptional methods, so the
// template's `[% IF authenticated %]` branch is the one that applies;
// the `[% ELSE %]` branch, "OK noauth\n" unconditionally, is for services
// with no authenticated methods at all and doesn't apply here):
//
//	sub auth_ping {
//	    my($self, $env) = @_;
//	    my $req = Plack::Request->new($env);
//	    my $token = $req->header("Authorization");
//	    if (!$token) {
//	        return [401, [], ["Authentication required"]];
//	    }
//	    my $auth_token = P3AuthToken->new(token => $token, ignore_authrc => 1);
//	    my($valid, $validate_err) = $self->validator->validate($auth_token);
//	    if ($valid) {
//	        return [200, ["Content-type" => "text/plain"], ["OK " . $auth_token->user_id . "\n"]];
//	    } else {
//	        warn "Token validation error $validate_err\n";
//	        return [403, [], "Authentication failed"];
//	    }
//	}
//
// Status/body matrix this reproduces:
//
//	no Authorization header  -> 401, no Content-Type header, body "Authentication required" (no trailing newline)
//	header present, invalid  -> 403, no Content-Type header, body "Authentication failed"    (no trailing newline)
//	header present, valid    -> 200, Content-Type: text/plain, body "OK <user_id>\n"
//
// The 403 case's Perl body is a bare string ("Authentication failed"),
// not the single-element arrayref every other branch uses ([...]) -- a
// malformed PSGI response body by the PSGI spec's own rules. A second,
// independently-maintained copy of this exact method
// (app_service/lib/Bio/KBase/AppService/AsyncService.pm:186-210) has the
// well-formed ["Authentication failed"], which resolves the question of
// what was *meant*: the same status and the same text, with no trailing
// newline either way. This port sends that byte content and doesn't try
// to reproduce the malformed-array typo itself.
//
// Deliberately not restricted to GET/HEAD (unlike Ping above): every
// generated .psgi file in this codebase mounts /auth_ping the same way it
// mounts /ping (`mount "/auth_ping" => sub { $server->auth_ping(@_) }`),
// with no method filtering, and the Perl sub itself never inspects the
// request method either.
func (s *Server) handleAuthPing(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("Authorization")
	if token == "" {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "Authentication required")
		return
	}

	userID, err := s.validator.Validate(token)
	if err != nil {
		s.logger.Warn("token validation error", "err", err)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "Authentication failed")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK %s\n", userID)
}
