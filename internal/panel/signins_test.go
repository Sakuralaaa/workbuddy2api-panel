package panel

import "testing"

func TestSignInsInfoAndTokenJSON(t *testing.T) {
	info := `{"auth":{"accessToken":"at1","refreshToken":"rt1","expiresAt":1893456000000,"domain":"www.workbuddy.ai"},"account":{"uid":"u-1","nickname":"甲"}}`
	got, err := signInsAsCockpit([]byte(info))
	if err != nil || len(got) != 1 || got[0].UID != "u-1" || got[0].RefreshToken != "rt1" || got[0].ExpiresAt != 1893456000000 || got[0].Domain != "www.workbuddy.ai" {
		t.Fatalf(".info parse: %+v %v", got, err)
	}
	list := `[{"uid":"u-2","accessToken":"a","refreshToken":"r","expiresAt":1893456000},{"x":{"access_token":"b","refresh_token":"c","uid":"u-3"}}]`
	got, err = signInsAsCockpit([]byte(list))
	if err != nil || len(got) != 2 || got[0].ExpiresAt != 1893456000000 || got[1].UID != "u-3" {
		t.Fatalf("token json parse: %+v %v", got, err)
	}
	if _, err := signInsAsCockpit([]byte(`{"auth":{"accessToken":{"$wbEncrypted":true}}}`)); err != errSealedSignIn {
		t.Fatalf("sealed err=%v", err)
	}
	if _, err := signInsAsCockpit([]byte(`{"a":1}`)); err == nil {
		t.Fatal("expected error for no sign-in")
	}
	if looksCockpit([]cockpitAccount{{ID: "x"}}) {
		t.Fatal("empty cockpit should not look valid")
	}
}
