package devloop

import "testing"

func TestAllowReload(t *testing.T) {
	t.Parallel()
	const nonce = "N0NCE"
	tests := []struct{ name, policy, want string }{
		{
			name:   "Forge's default policy already allows same-origin scripts and fetches",
			policy: "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'",
			want:   "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'",
		},
		{
			name:   "a nonce-only policy gets the nonce and same-origin connections",
			policy: "default-src 'none'; script-src 'nonce-abc'; connect-src https://api.example.com",
			want:   "default-src 'none'; script-src 'nonce-abc' 'nonce-N0NCE'; connect-src https://api.example.com 'self'",
		},
		{
			name:   "strict-dynamic ignores 'self', so the nonce is needed",
			policy: "script-src 'self' 'strict-dynamic' 'nonce-abc'",
			want:   "script-src 'self' 'strict-dynamic' 'nonce-abc' 'nonce-N0NCE'",
		},
		{
			name:   "script-src-elem takes precedence over script-src",
			policy: "script-src 'self'; script-src-elem https://cdn.example.com",
			want:   "script-src 'self'; script-src-elem https://cdn.example.com 'nonce-N0NCE'",
		},
		{
			name:   "default-src alone is narrowed to the two directives that matter",
			policy: "default-src 'none'",
			want:   "default-src 'none'; script-src-elem 'nonce-N0NCE'; connect-src 'self'",
		},
		{
			name:   "default-src sources are kept in the new directives",
			policy: "default-src https://cdn.example.com",
			want:   "default-src https://cdn.example.com; script-src-elem https://cdn.example.com 'nonce-N0NCE'; connect-src https://cdn.example.com 'self'",
		},
		{
			name:   "a policy that restricts neither is untouched",
			policy: "frame-ancestors 'none'; img-src 'self'",
			want:   "frame-ancestors 'none'; img-src 'self'",
		},
		{
			name:   "unsafe-inline keeps working: the script is admitted by origin, not by nonce",
			policy: "script-src 'unsafe-inline'; connect-src 'self'",
			want:   "script-src 'unsafe-inline' 'self'; connect-src 'self'",
		},
		{
			name:   "unsafe-inline under default-src",
			policy: "default-src 'unsafe-inline'",
			want:   "default-src 'unsafe-inline'; script-src-elem 'unsafe-inline' 'self'; connect-src 'unsafe-inline' 'self'",
		},
		{
			name:   "a nonce already disabled unsafe-inline, so another nonce is harmless",
			policy: "script-src 'unsafe-inline' 'nonce-abc'",
			want:   "script-src 'unsafe-inline' 'nonce-abc' 'nonce-N0NCE'",
		},
		{
			name:   "wildcards already allow the stream",
			policy: "script-src *; connect-src *",
			want:   "script-src *; connect-src *",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := allowReload(test.policy, nonce); got != test.want {
				t.Errorf("allowReload(%q)\n got %q\nwant %q", test.policy, got, test.want)
			}
		})
	}
}
