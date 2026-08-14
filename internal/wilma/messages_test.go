package wilma

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		raw      string
		wantZero bool
	}{
		{"2026-08-11 09:14:00", false},
		{"2026-08-11 09:14", false},
		{"not a timestamp", true},
		{"", true},
	}
	for _, tc := range cases {
		got := parseTimestamp(tc.raw)
		if got.IsZero() != tc.wantZero {
			t.Errorf("parseTimestamp(%q).IsZero() = %v, want %v (got %v)", tc.raw, got.IsZero(), tc.wantZero, got)
		}
	}

	got := parseTimestamp("2026-08-11 09:14:00")
	want := time.Date(2026, 8, 11, 9, 14, 0, 0, Helsinki)
	if !got.Equal(want) {
		t.Errorf("parseTimestamp seconds form = %v, want %v", got, want)
	}
}

func TestMessageMarshalOmitsZeroSentAt(t *testing.T) {
	m := Message{ID: 1, SentAtRaw: "garbage"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"sent_at"`) {
		t.Errorf("expected sent_at to be omitted for zero time, got: %s", b)
	}
	if !strings.Contains(string(b), `"sent_at_raw":"garbage"`) {
		t.Errorf("expected sent_at_raw to be preserved, got: %s", b)
	}
}

// TestLoginListFetchRoundTrip exercises login -> role discovery -> list ->
// fetch against a fake Wilma server modeled on the real (HTML-heavy,
// non-JSON-login) protocol, without any network access.
func TestLoginListFetchRoundTrip(t *testing.T) {
	const rootHTML = `<!DOCTYPE html><html><body>
<a class="text-style-link" href="/!111/">Aino Virtanen <small>Koulu</small></a>
<a class="text-style-link" href="/!222/">Vaino Virtanen <small>Koulu</small></a>
<form action="/logout" method="post"><input type="hidden" name="formkey" value="passwd:1:abc123"></form>
</body></html>`

	const detailHTML = `<!DOCTYPE html><html><body>
<div id="recipients-cell" class="margin-bottom-inline"><a href="/x">8A huoltajat</a></div>
<div class="ckeditor hidden"><p>Hei</p><div class="nested">sisäkkäin</div></div>
</body></html>`

	loggedIn := false
	mux := http.NewServeMux()
	mux.HandleFunc("/index_json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"SessionID":"sess123"}`))
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("SESSIONID") != "sess123" {
			t.Errorf("login got SESSIONID=%q, want sess123", r.FormValue("SESSIONID"))
		}
		http.SetCookie(w, &http.Cookie{Name: "Wilma2SID", Value: "cookieval"})
		loggedIn = true
		http.Redirect(w, r, "/?checkcookie", http.StatusSeeOther)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !loggedIn {
			t.Fatal("root page requested before login")
		}
		w.Write([]byte(rootHTML))
	})
	mux.HandleFunc("/!111/messages/list", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Messages":[{"Id":501,"Subject":"Retki","TimeStamp":"2026-08-11 09:14:00","Sender":"Opettaja","SenderId":9,"Status":1}]}`))
	})
	mux.HandleFunc("/!111/messages/501", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(detailHTML))
	})
	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		if got := r.FormValue("formkey"); got != "passwd:1:abc123" {
			t.Errorf("logout got formkey=%q", got)
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	client, err := NewClient(host, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Point the client at the httptest server's actual scheme (http, not https).
	client.baseURL.Scheme = "http"
	client.baseURL.Host = host

	roles, err := client.LoginAndDiscoverRoles("parent", "secret")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("got %d roles, want 2: %+v", len(roles), roles)
	}
	if roles[0].Prefix != "/!111" || roles[0].Name != "Aino Virtanen" {
		t.Errorf("unexpected first role: %+v", roles[0])
	}

	list, err := client.ListFolder(roles[0].Prefix, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	msgs := ToMessages(roles[0].Prefix, roles[0].Name, host, list)
	if len(msgs) != 1 || msgs[0].ID != 501 {
		t.Fatalf("unexpected messages: %+v", msgs)
	}

	if err := FillBody(client, roles[0].Prefix, &msgs[0]); err != nil {
		t.Fatalf("fill body: %v", err)
	}
	if msgs[0].BodyText != "Hei\n\nsisäkkäin" {
		t.Errorf("body text = %q, want %q", msgs[0].BodyText, "Hei\n\nsisäkkäin")
	}
	if len(msgs[0].Recipients) != 1 || msgs[0].Recipients[0] != "8A huoltajat" {
		t.Errorf("recipients = %+v", msgs[0].Recipients)
	}

	if err := client.Logout(); err != nil {
		t.Errorf("logout: %v", err)
	}
}

// TestFetchMessage_LoginRedirectIsSessionExpired confirms that a session
// that has expired between listing and fetching a message body — which
// shows up only as a redirect to the login page, since the detail page has
// no JSON error envelope to inspect — is reported as ErrSessionExpired
// rather than the generic "could not find message body" error.
func TestFetchMessage_LoginRedirectIsSessionExpired(t *testing.T) {
	const loginHTML = `<!DOCTYPE html><html><body>
<form action="/login" method="post">
<input type="text" name="Login">
<input type="password" name="Password">
</form>
</body></html>`

	mux := http.NewServeMux()
	mux.HandleFunc("/!111/messages/501", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(loginHTML))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	client, err := NewClient(host, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL.Scheme = "http"
	client.baseURL.Host = host

	_, err = client.FetchMessage("/!111", 501)
	if err == nil {
		t.Fatal("FetchMessage: want error, got nil")
	}
	if !errors.Is(err, ErrSessionExpired) {
		t.Errorf("FetchMessage error = %v, want errors.Is(err, ErrSessionExpired)", err)
	}
}
