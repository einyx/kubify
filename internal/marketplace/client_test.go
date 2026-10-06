package marketplace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResolveSendsMarketplaceToken(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		if r.URL.Path == "/token" { json.NewEncoder(w).Encode(map[string]interface{}{"access_token":"publisher-token","expires_in":3600}); return }
		got=r.Header.Get("x-ms-marketplace-token"); json.NewEncoder(w).Encode(map[string]interface{}{"id":"sub-1","planId":"full","subscription":map[string]interface{}{"purchaser":map[string]string{"tenantId":"tenant-1"}}})
	})); defer ts.Close()
	c,_:=New(Config{TenantID:"tenant",ClientID:"client",ClientSecret:"secret",APIBase:ts.URL,HTTPClient:ts.Client()})
	// Seed the token because Entra's hostname is intentionally fixed in production.
	c.token="publisher-token"; c.expires=time.Now().Add(time.Hour)
	r,err:=c.Resolve(context.Background(),"purchase+/token"); if err!=nil{t.Fatal(err)}
	if got!="purchase+/token"||r.ID!="sub-1"{t.Fatalf("token=%q response=%+v",got,r)}
}
