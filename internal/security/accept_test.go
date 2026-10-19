package security

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAcceptingScanner(t *testing.T) {
	zeros := strings.Repeat("0", 32)
	placeholder := `private const string OfflineToken = "` + zeros + `";`
	ghToken := "ghp_1234567890abcdefghijklmnopqrstuvwxyzAB"
	offline := []Accept{{
		Rule:       "generic-credential-assignment",
		Identifier: "token",
		Value:      zeros,
		Paths:      []string{"src/**/OfflineAuth.cs"},
		Reason:     "offline placeholder",
	}}
	ghAccept := []Accept{{
		Rule:       "generic-credential-assignment",
		Identifier: "token",
		Value:      ghToken,
		Paths:      []string{"src/**/OfflineAuth.cs"},
		Reason:     "offline placeholder",
	}}
	awsAccept := []Accept{{
		Rule:   "aws-access-key-id",
		Value:  "AKIAIOSFODNN7EXAMPLE",
		Paths:  []string{"docs/**"},
		Reason: "AWS documentation example key",
	}}
	const path = "src/Auth/OfflineAuth.cs"

	cases := []struct {
		name      string
		accepts   []Accept
		path      string
		content   string
		wantRule  string // "" means expect no findings
		wantCount int
	}{
		{
			name:    "accepted placeholder dropped",
			accepts: offline,
			path:    path,
			content: placeholder,
		},
		{
			name:    "plain keyword case-folded dropped",
			accepts: offline,
			path:    path,
			content: `token = "` + zeros + `"`,
		},
		{
			name:      "different value same identifier kept",
			accepts:   offline,
			path:      path,
			content:   `OfflineToken = "a1b2c3d4e5f6a7b8c9d0e1f2"`,
			wantRule:  "generic-credential-assignment",
			wantCount: 1,
		},
		{
			name:      "second credential in the file kept",
			accepts:   offline,
			path:      path,
			content:   placeholder + "\nvar key = AKIAQYLPMN5HXYZ12345;",
			wantRule:  "aws-access-key-id",
			wantCount: 1,
		},
		{
			name:      "provider rule not suppressed by a generic entry",
			accepts:   ghAccept,
			path:      path,
			content:   `token = "` + ghToken + `"`,
			wantRule:  "github-token",
			wantCount: 1,
		},
		{
			name:      "pathless scan never exempt",
			accepts:   offline,
			path:      "",
			content:   placeholder,
			wantRule:  "generic-credential-assignment",
			wantCount: 1,
		},
		{
			name:      "non-matching path kept",
			accepts:   offline,
			path:      "src/Other.cs",
			content:   placeholder,
			wantRule:  "generic-credential-assignment",
			wantCount: 1,
		},
		{
			name:      "same value under another keyword kept",
			accepts:   offline,
			path:      path,
			content:   `password = "` + zeros + `"`,
			wantRule:  "generic-credential-assignment",
			wantCount: 1,
		},
		{
			name:    "provider entry drops its key",
			accepts: awsAccept,
			path:    "docs/aws.md",
			content: "export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:      "no accepts behaves like RegexScanner",
			accepts:   nil,
			path:      path,
			content:   placeholder,
			wantRule:  "generic-credential-assignment",
			wantCount: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := NewScanner(tc.accepts).Scan(tc.path, tc.content)
			require.NoError(t, err)
			require.Len(t, findings, tc.wantCount)
			if tc.wantRule == "" {
				require.Nil(t, findings)

				return
			}
			require.Equal(t, tc.wantRule, findings[0].Rule)
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	generic := Accept{
		Rule:       "generic-credential-assignment",
		Identifier: "token",
		Value:      strings.Repeat("0", 32),
		Paths:      []string{"src/**/OfflineAuth.cs"},
		Reason:     "offline placeholder",
	}
	aws := Accept{
		Rule:   "aws-access-key-id",
		Value:  "AKIAIOSFODNN7EXAMPLE",
		Paths:  []string{"docs/**"},
		Reason: "AWS documentation example key",
	}
	with := func(base Accept, edit func(*Accept)) Accept {
		edit(&base)

		return base
	}

	cases := []struct {
		name    string
		accepts []Accept
		wantErr string // "" means valid
	}{
		{name: "valid generic", accepts: []Accept{generic}},
		{name: "valid provider", accepts: []Accept{aws}},
		{name: "valid both", accepts: []Accept{generic, aws}},
		{
			name:    "unknown rule",
			accepts: []Accept{with(aws, func(a *Accept) { a.Rule = "nope" })},
			wantErr: `security: accept #1: unknown rule "nope"`,
		},
		{
			name:    "empty value",
			accepts: []Accept{with(aws, func(a *Accept) { a.Value = "  " })},
			wantErr: "empty value",
		},
		{
			name:    "empty reason",
			accepts: []Accept{with(aws, func(a *Accept) { a.Reason = "" })},
			wantErr: "empty reason",
		},
		{
			name:    "no paths",
			accepts: []Accept{with(aws, func(a *Accept) { a.Paths = nil })},
			wantErr: "no paths",
		},
		{
			name:    "invalid path glob",
			accepts: []Accept{with(aws, func(a *Accept) { a.Paths = []string{"docs/[a"} })},
			wantErr: "invalid path glob",
		},
		{
			name:    "generic missing identifier",
			accepts: []Accept{with(generic, func(a *Accept) { a.Identifier = "" })},
			wantErr: "is not a generic-credential-assignment keyword",
		},
		{
			name:    "generic identifier outside keyword set",
			accepts: []Accept{with(generic, func(a *Accept) { a.Identifier = "OfflineToken" })},
			wantErr: `identifier "OfflineToken" is not`,
		},
		{
			name:    "generic value too short",
			accepts: []Accept{with(generic, func(a *Accept) { a.Value = "000000000000000" })},
			wantErr: "value shorter than 16",
		},
		{
			name:    "generic value with quote",
			accepts: []Accept{with(generic, func(a *Accept) { a.Value = `0000000000000000"` })},
			wantErr: "value contains a quote",
		},
		{
			name:    "provider with identifier",
			accepts: []Accept{with(aws, func(a *Accept) { a.Identifier = "token" })},
			wantErr: "identifier is only valid for",
		},
		{
			name:    "provider value not a full match",
			accepts: []Accept{with(aws, func(a *Accept) { a.Value = "key AKIAIOSFODNN7EXAMPLE" })},
			wantErr: "value is not a full match",
		},
		{
			name:    "duplicate entry case-folded identifier",
			accepts: []Accept{generic, aws, with(generic, func(a *Accept) { a.Identifier = "TOKEN" })},
			wantErr: "security: accept #3: duplicate entry",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAccepts(tc.accepts)
			if tc.wantErr == "" {
				require.NoError(t, err)

				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
