package sources

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OAuth1 holds OAuth 1.0a user-context credentials (HMAC-SHA1).
type OAuth1 struct {
	ConsumerKey, ConsumerSecret, Token, TokenSecret string
}

// header returns the Authorization header for a request.
func (o OAuth1) header(method, baseURL string, query url.Values) string {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce) // crypto/rand.Read never returns an error
	return o.headerWith(method, baseURL, query, hex.EncodeToString(nonce), strconv.FormatInt(time.Now().Unix(), 10))
}

func (o OAuth1) headerWith(method, baseURL string, params url.Values, nonce, ts string) string {
	oauth := map[string]string{
		"oauth_consumer_key":     o.ConsumerKey,
		"oauth_nonce":            nonce,
		"oauth_signature_method": "HMAC-SHA1",
		"oauth_timestamp":        ts,
		"oauth_token":            o.Token,
		"oauth_version":          "1.0",
	}
	var pairs []string
	for k, vs := range params {
		for _, v := range vs {
			pairs = append(pairs, pctEncode(k)+"="+pctEncode(v))
		}
	}
	for k, v := range oauth {
		pairs = append(pairs, pctEncode(k)+"="+pctEncode(v))
	}
	sort.Strings(pairs)
	base := strings.ToUpper(method) + "&" + pctEncode(baseURL) + "&" + pctEncode(strings.Join(pairs, "&"))
	mac := hmac.New(sha1.New, []byte(pctEncode(o.ConsumerSecret)+"&"+pctEncode(o.TokenSecret)))
	mac.Write([]byte(base))
	oauth["oauth_signature"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))

	keys := make([]string, 0, len(oauth))
	for k := range oauth {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = pctEncode(k) + `="` + pctEncode(oauth[k]) + `"`
	}
	return "OAuth " + strings.Join(parts, ", ")
}

// pctEncode is RFC 3986 percent-encoding as required by OAuth 1.0a.
func pctEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}
