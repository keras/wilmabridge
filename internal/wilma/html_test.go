package wilma

import "testing"

func TestHTMLToText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "paragraphs and br",
			in:   "<p>Hei,</p><p>Retki on ma&auml;nantaina.<br>Muista eväät.</p>",
			want: "Hei,\n\nRetki on maänantaina.\nMuista eväät.",
		},
		{
			name: "list items",
			in:   "<ul><li>Eväät</li><li>Saappaat</li></ul>",
			want: "- Eväät\n- Saappaat",
		},
		{
			name: "script and style stripped",
			in:   "<style>p{color:red}</style><p>Näkyvä teksti</p><script>alert(1)</script>",
			want: "Näkyvä teksti",
		},
		{
			name: "entities unescaped",
			in:   "<p>&Auml;iti &amp; Is&auml; &lt;3</p>",
			want: "Äiti & Isä <3",
		},
		{
			name: "collapses excess blank lines",
			in:   "<p>A</p><p></p><p></p><p>B</p>",
			want: "A\n\nB",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := htmlToText(tc.in)
			if got != tc.want {
				t.Errorf("htmlToText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
