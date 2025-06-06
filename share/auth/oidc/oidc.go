// Package oidc implements OpenID Connect client logic for the golang.org/x/oauth2 package.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mitchellh/pointerstructure"
	"golang.org/x/oauth2"
)

const (
	// ScopeOpenID is the mandatory scope for all OpenID Connect OAuth2 requests.
	ScopeOpenID = "openid"

	// ScopeOfflineAccess is an optional scope defined by OpenID Connect for requesting
	// OAuth2 refresh tokens.
	//
	// Support for this scope differs between OpenID Connect providers. For instance
	// Google rejects it, favoring appending "access_type=offline" as part of the
	// authorization request instead.
	//
	// See: https://openid.net/specs/openid-connect-core-1_0.html#OfflineAccess
	ScopeOfflineAccess = "offline_access"

	oidcClaimNames  = "_claim_names"
	oidcGroups      = "groups"
	oidcClaimSource = "_claim_sources"

	oidcGraphWindowsNet   = "graph.windows.net"
	oidcGraphMicrosoftCom = "graph.microsoft.com"

	oidcGraphMicrosoftAzureUs = "graph.microsoftazure.us"
	oidcGraphMicrosoftUs      = "graph.microsoft.us"
)

// ClientContext returns a new Context that carries the provided HTTP client.
//
// This method sets the same context key used by the golang.org/x/oauth2 package,
// so the returned context works for that package too.
//
//	myClient := &http.Client{}
//	ctx := oidc.ClientContext(parentContext, myClient)
//
//	// This will use the custom client
//	provider, err := oidc.NewProvider(ctx, "https://accounts.example.com")
func ClientContext(ctx context.Context, client *http.Client) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, client)
}

func doRequest(ctx context.Context, req *http.Request) (*http.Response, error) {
	var client *http.Client
	if c, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		client = c
	} else {
		// We should have assigned oauth2.HTTPClient in all paths.
		// For compliance reason, we don't fallback.
		return nil, errors.New("failed to find http.Client")
	}
	return client.Do(req.WithContext(ctx))
}

// Provider represents an OpenID Connect server's configuration.
type Provider struct {
	Issuer      string
	AuthURL     string
	TokenURL    string
	UserInfoURL string

	// Raw claims returned by the server.
	rawClaims []byte

	remoteKeySet *remoteKeySet
}

type providerJSON struct {
	Issuer      string `json:"issuer"`
	AuthURL     string `json:"authorization_endpoint"`
	TokenURL    string `json:"token_endpoint"`
	JWKSURL     string `json:"jwks_uri"`
	UserInfoURL string `json:"userinfo_endpoint"`
}

type Endpoints struct {
	providerJSON
}

// NewProvider uses the OpenID Connect discovery mechanism to construct a Provider.
//
// The issuer is the URL identifier for the service. For example: "https://accounts.google.com"
// or "https://login.salesforce.com".
func Discover(ctx context.Context, issuer string) (*Endpoints, error) {
	wellKnown := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequest("GET", wellKnown, nil)
	if err != nil {
		return nil, err
	}
	resp, err := doRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("unable to read response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, body)
	}

	var p providerJSON
	err = unmarshalResp(resp, body, &p)
	if err != nil {
		return nil, fmt.Errorf("oidc: failed to decode provider discovery object: %v", err)
	}

	if p.Issuer != issuer {
		return nil, fmt.Errorf("oidc: issuer did not match the issuer returned by provider, expected %q got %q", issuer, p.Issuer)
	}

	return &Endpoints{providerJSON: p}, nil
}

// Claims unmarshals raw fields returned by the server during discovery.
//
//	var claims struct {
//	    ScopesSupported []string `json:"scopes_supported"`
//	    ClaimsSupported []string `json:"claims_supported"`
//	}
//
//	if err := provider.Claims(&claims); err != nil {
//	    // handle unmarshaling error
//	}
//
// For a list of fields defined by the OpenID Connect spec see:
// https://openid.net/specs/openid-connect-discovery-1_0.html#ProviderMetadata
func (p *Provider) Claims(v interface{}) error {
	if p.rawClaims == nil {
		return errors.New("oidc: claims not set")
	}
	return json.Unmarshal(p.rawClaims, v)
}

// Endpoint returns the OAuth2 auth and token endpoints for the given provider.
func (p *Provider) Endpoint() oauth2.Endpoint {
	return oauth2.Endpoint{AuthURL: p.AuthURL, TokenURL: p.TokenURL}
}

// UserInfo represents the OpenID Connect userinfo claims.
type UserInfo struct {
	Subject       string `json:"sub"`
	Profile       string `json:"profile"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`

	claims []byte
}

// Claims unmarshals the raw JSON object claims into the provided object.
func (u *UserInfo) Claims(v interface{}) error {
	if u.claims == nil {
		return errors.New("oidc: claims not set")
	}
	return json.Unmarshal(u.claims, v)
}

// UserInfo uses the token source to query the provider's user info endpoint.
func UserInfoReq(ctx context.Context, userInfoURL string, tokenSource oauth2.TokenSource) (*UserInfo, error) {
	if userInfoURL == "" {
		return nil, errors.New("oidc: user info endpoint is not supported by this provider")
	}

	req, err := http.NewRequest("GET", userInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("oidc: create GET request: %v", err)
	}

	token, err := tokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("oidc: get access token: %v", err)
	}
	token.SetAuthHeader(req)

	resp, err := doRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, body)
	}

	var userInfo UserInfo
	if err := json.Unmarshal(body, &userInfo); err != nil {
		return nil, fmt.Errorf("oidc: failed to decode userinfo: %v", err)
	}
	userInfo.claims = body
	return &userInfo, nil
}

// GetAzureGroupInfo gets Azure's group information following _claim_sources.
func GetAzureGroupInfo(ctx context.Context, allClaims map[string]interface{}, tokenSource oauth2.TokenSource) (interface{}, error) {

	// Here we check if below conditions are met:
	// 1. claims.iss contains "login.microsoftonline.com", which means it's coming from Azure AD.
	// 2. No groups is provided.
	// 3. _claim_sources is available.
	// If they're all met at the same time, we try to get information from the endpoint specified in _claim_sources.
	iss, err := pointerstructure.Get(allClaims, "/iss")
	if err != nil {
		return nil, fmt.Errorf("failed to find issuer: %w", err)
	}
	issuer, ok := iss.(string)
	if !ok {
		return nil, fmt.Errorf("invalid issuer: %v", iss)
	}

	if !strings.Contains(issuer, "login.microsoftonline.com") {
		return nil, fmt.Errorf("not recognized issuer: %s", issuer)
	}

	src, err := pointerstructure.Get(allClaims, fmt.Sprintf("/%s/%s", oidcClaimNames, oidcGroups))
	if err != nil {
		return nil, fmt.Errorf("failed to find group claim name: %w", err)
	}

	srcname, ok := src.(string)
	if !ok {
		return nil, fmt.Errorf("invalid srcname: %v", src)
	}

	endpointPath := fmt.Sprintf("/%s/%s/endpoint", oidcClaimSource, srcname)
	endpoint, err := pointerstructure.Get(allClaims, endpointPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find endpoint path: %w", err)
	}

	groupUrl, ok := endpoint.(string)
	if !ok {
		return nil, fmt.Errorf("invalid endpoint: %v", endpoint)
	}

	urlParsed, err := url.Parse(groupUrl)
	if err != nil {
		return nil, fmt.Errorf("failed to parse group url: %w", err)
	}

	if urlParsed.Host == oidcGraphWindowsNet {
		urlParsed.Host = oidcGraphMicrosoftCom
		urlParsed.Path = "/v1.0" + urlParsed.Path
	} else if urlParsed.Host == oidcGraphMicrosoftAzureUs {
		urlParsed.Host = oidcGraphMicrosoftUs
		urlParsed.Path = "/v1.0" + urlParsed.Path
	}

	payload := strings.NewReader("{\"securityEnabledOnly\": false}")
	req, err := http.NewRequest("POST", urlParsed.String(), payload)
	if err != nil {
		return nil, fmt.Errorf("failed to create POST request: %w", err)
	}

	req.Header.Add("content-type", "application/json")

	token, err := tokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("failed to create access token: %w", err)
	}
	token.SetAuthHeader(req)

	resp, err := doRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read resp body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP code %s: %s", resp.Status, body)
	}

	target := struct {
		Value []interface{} `json:"value"`
	}{}

	if err := json.Unmarshal(body, &target); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return target.Value, nil
}

// IDToken is an OpenID Connect extension that provides a predictable representation
// of an authorization event.
//
// The ID Token only holds fields OpenID Connect requires. To access additional
// claims returned by the server, use the Claims method.
type IDToken struct {
	// The URL of the server which issued this token. OpenID Connect
	// requires this value always be identical to the URL used for
	// initial discovery.
	//
	// Note: Because of a known issue with Google Accounts' implementation
	// this value may differ when using Google.
	//
	// See: https://developers.google.com/identity/protocols/OpenIDConnect#obtainuserinfo
	Issuer string

	// The client ID, or set of client IDs, that this token is issued for. For
	// common uses, this is the client that initialized the auth flow.
	//
	// This package ensures the audience contains an expected value.
	Audience []string

	// A unique string which identifies the end user.
	Subject string

	// Expiry of the token. Ths package will not process tokens that have
	// expired unless that validation is explicitly turned off.
	Expiry time.Time
	// When the token was issued by the provider.
	IssuedAt time.Time

	// Initial nonce provided during the authentication redirect.
	//
	// This package does NOT provided verification on the value of this field
	// and it's the user's responsibility to ensure it contains a valid value.
	Nonce string

	// Raw payload of the id_token.
	claims []byte
}

// Claims unmarshals the raw JSON payload of the ID Token into a provided struct.
//
//	idToken, err := idTokenVerifier.Verify(rawIDToken)
//	if err != nil {
//		// handle error
//	}
//	var claims struct {
//		Email         string `json:"email"`
//		EmailVerified bool   `json:"email_verified"`
//	}
//	if err := idToken.Claims(&claims); err != nil {
//		// handle error
//	}
func (i *IDToken) Claims() (map[string]interface{}, error) {
	if i.claims == nil {
		return nil, errors.New("oidc: claims not set")
	}

	var a interface{}
	if err := json.Unmarshal(i.claims, &a); err != nil {
		return nil, err
	}
	if m, ok := a.(map[string]interface{}); ok {
		return m, nil
	}

	return nil, errors.New("oidc: unable to parse claims")
}

type idToken struct {
	Issuer   string   `json:"iss"`
	Subject  string   `json:"sub"`
	Audience audience `json:"aud"`
	Expiry   jsonTime `json:"exp"`
	IssuedAt jsonTime `json:"iat"`
	Nonce    string   `json:"nonce"`
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*a = audience{s}
		return nil
	}
	var auds []string
	if err := json.Unmarshal(b, &auds); err != nil {
		return err
	}
	*a = audience(auds)
	return nil
}

type jsonTime time.Time

func (j *jsonTime) UnmarshalJSON(b []byte) error {
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	var unix int64

	if t, err := n.Int64(); err == nil {
		unix = t
	} else {
		f, err := n.Float64()
		if err != nil {
			return err
		}
		unix = int64(f)
	}
	*j = jsonTime(time.Unix(unix, 0))
	return nil
}

func unmarshalResp(r *http.Response, body []byte, v interface{}) error {
	err := json.Unmarshal(body, &v)
	if err == nil {
		return nil
	}
	ct := r.Header.Get("Content-Type")
	mediaType, _, parseErr := mime.ParseMediaType(ct)
	if parseErr == nil && mediaType == "application/json" {
		return fmt.Errorf("got Content-Type = application/json, but could not unmarshal as JSON: %v", err)
	}
	return fmt.Errorf("expected Content-Type = application/json, got %q: %v", ct, err)
}
