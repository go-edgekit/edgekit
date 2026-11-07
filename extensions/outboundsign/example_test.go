package outboundsign_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/outboundsign"
)

func ExampleNew() {
	// In a deployment, the key comes from a secret store.
	key := []byte("0123456789abcdef0123456789abcdef")

	// The upstream checks the signature of the request that it got.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mac := hmac.New(sha256.New, key)
		_, _ = io.WriteString(mac, r.Method+"\nhttp\n"+r.Host+"\n"+r.Host+"\n"+r.RequestURI)
		valid := hmac.Equal([]byte(r.Header.Get(outboundsign.SignatureHeader)), []byte(hex.EncodeToString(mac.Sum(nil))))
		fmt.Println("upstream got", r.RequestURI, "with a valid signature:", valid)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL + "/v1")
	if err != nil {
		fmt.Println(err)

		return
	}

	selector, err := edgekit.StaticUpstream(target)
	if err != nil {
		fmt.Println(err)

		return
	}

	signer := outboundsign.New()
	withKey, err := signer.WithKey(key)
	if err != nil {
		fmt.Println(err)

		return
	}

	// The signer is the last hook provider, so it signs after all other
	// changes. The route middleware gives it the key of the route.
	gateway, err := edgekit.New(edgekit.Config{
		Hooks: []any{
			signer,
		},
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: selector,
			Middleware: []edgekit.Middleware{
				withKey,
			},
		}},
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	r := httptest.NewRequest(http.MethodGet, "/api/items?page=2", nil)
	r.Header.Set(outboundsign.SignatureHeader, "forged by the client")

	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, r)
	fmt.Println("status:", w.Code)

	// Output:
	// upstream got /v1/api/items?page=2 with a valid signature: true
	// status: 204
}
