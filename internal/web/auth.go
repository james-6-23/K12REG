package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const cookieName = "k12_session"

type auth struct {
	password  string
	maxAge    time.Duration
	key       []byte
}

func newAuth(password string, sessionDays int) *auth {
	if sessionDays < 1 {
		sessionDays = 30
	}
	material := []byte("k12reg-session-v1:" + password)
	sum := sha256.Sum256(material)
	return &auth{
		password: password,
		maxAge:   time.Duration(sessionDays) * 24 * time.Hour,
		key:      sum[:],
	}
}

func (a *auth) issue() (string, error) {
	exp := time.Now().Add(a.maxAge).Unix()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload := fmt.Sprintf("%d.%s", exp, base64.RawURLEncoding.EncodeToString(nonce))
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return payload + "." + sig, nil
}

func (a *auth) valid(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	expS, nonce, sig := parts[0], parts[1], parts[2]
	if expS == "" || nonce == "" || sig == "" {
		return false
	}
	exp, err := strconv.ParseInt(expS, 10, 64)
	if err != nil || exp < time.Now().Unix() {
		return false
	}
	payload := expS + "." + nonce
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(payload))
	expect := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expect), []byte(sig))
}

func (a *auth) checkPassword(pw string) bool {
	// constant-time via hashed compare
	ha := sha256.Sum256([]byte(pw))
	hb := sha256.Sum256([]byte(a.password))
	return hmac.Equal(ha[:], hb[:])
}

func (a *auth) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(a.maxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *auth) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *auth) fromRequest(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func (a *auth) require(w http.ResponseWriter, r *http.Request) bool {
	if !a.valid(a.fromRequest(r)) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"detail": "未登录"})
		return false
	}
	return true
}
