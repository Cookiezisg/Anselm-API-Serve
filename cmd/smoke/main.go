// Command smoke exercises the paid endpoints against a REAL deployment with a
// REAL credential, and reports what each one cost.
//
// It exists because these endpoints cannot be smoke-tested by hand: every
// protected call carries an Ed25519 proof signed over the exact request body, so
// `curl` cannot reach them at all. What it does is what a desktop client does —
// register an install, fetch a nonce, sign, send — with the money made visible.
//
// **It spends the operator's real money.** Nothing runs without -yes.
//
// smoke 对着**真实部署**用**真实凭证**跑通付费端点,并报告每一条花了多少。
//
// 它之所以存在,是因为这些端点**没法手工冒烟**:每个受保护调用都带一个覆盖精确请求体的 Ed25519
// proof,`curl` 根本够不着。它做的就是桌面端做的事——登记 install、取 nonce、签名、发送——只不过
// 把钱显式打出来。
//
// **它花的是 operator 的真钱。** 没有 -yes 什么都不跑。
//
// Usage:
//
//	go run ./cmd/smoke -base https://<gateway-host> -run chat,image,edit,speech,video -yes
//	go run ./cmd/smoke -base https://<gateway-host> -run voice -sample ./voice.wav -yes
//
// The identity is persisted (-identity, default ./smoke-identity.json) so repeat
// runs reuse ONE install rather than minting a new one each time — otherwise the
// install-issuance gates are what you end up testing.
//
// 身份会落盘(-identity,默认 ./smoke-identity.json),使重复运行复用**同一个** install,
// 而不是每次都领一个新号——否则你测的就变成领号闸了。
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	headerInstallID = "X-Anselm-Install-ID"
	headerProof     = "X-Anselm-Proof"
	headerPublicKey = "X-Anselm-Public-Key"
	headerOffset    = "Upload-Offset"
)

var (
	base      = flag.String("base", "", "gateway origin, e.g. https://gateway.example.com (required)")
	run       = flag.String("run", "chat,image,edit,speech,video", "comma-separated steps: chat,image,edit,speech,video,voice")
	yes       = flag.Bool("yes", false, "actually spend real money (without this, nothing is sent)")
	identity  = flag.String("identity", "smoke-identity.json", "where to persist the install identity")
	sample    = flag.String("sample", "", "wav/mp3 voice sample, required by -run voice")
	source    = flag.String("image", "", "source image for the edit step (defaults to the image the image step just made)")
	pollFor   = flag.Duration("poll", 5*time.Minute, "how long to poll a submitted video before giving up")
	voiceName = flag.String("voice-name", "", "name for the enrolled voice (default: smoke-<timestamp>)")
)

func main() {
	flag.Parse()
	if strings.TrimSpace(*base) == "" {
		fail("-base is required (the gateway origin; it is deliberately not compiled in)")
	}
	*base = strings.TrimRight(*base, "/")

	steps := map[string]bool{}
	for _, s := range strings.Split(*run, ",") {
		if s = strings.TrimSpace(s); s != "" {
			steps[s] = true
		}
	}
	if steps["voice"] && strings.TrimSpace(*sample) == "" {
		fail("-run voice needs -sample <a real wav/mp3 voice clip>")
	}

	if !*yes {
		fmt.Println("DRY RUN — nothing was sent. These steps would spend real money:")
		for _, s := range []string{"chat", "image", "edit", "speech", "video", "voice"} {
			if steps[s] {
				fmt.Printf("  %-7s %s\n", s, costHint[s])
			}
		}
		fmt.Println("\nRe-run with -yes to actually spend it.")
		return
	}

	c := newClient()
	fmt.Printf("gateway  %s\n", *base)
	fmt.Printf("install  %s\n\n", c.installID)

	before := c.quota()
	fmt.Printf("quota before   used=%d remaining=%d\n\n", before.Used, before.Remaining)

	var madeImage string
	order := []string{"chat", "image", "edit", "speech", "video", "voice"}
	for _, name := range order {
		if !steps[name] {
			continue
		}
		start := time.Now()
		var err error
		switch name {
		case "chat":
			err = c.chat()
		case "image":
			madeImage, err = c.image()
		case "edit":
			src := *source
			if src == "" {
				src = madeImage
			}
			err = c.edit(src)
		case "speech":
			err = c.speech()
		case "video":
			err = c.video()
		case "voice":
			err = c.voice()
		}
		status := "OK"
		if err != nil {
			status = "FAIL " + err.Error()
		}
		fmt.Printf("  %-7s %-6s %s\n\n", name, time.Since(start).Round(time.Millisecond), status)
	}

	after := c.quota()
	fmt.Printf("quota after    used=%d remaining=%d   (this run consumed %d requests)\n",
		after.Used, after.Remaining, after.Used-before.Used)
	fmt.Println("\nThe pUSD wallet is NOT in this view — read it on the dashboard (loopback :8081).")
}

var costHint = map[string]string{
	"chat":   "1 chat request (tokens)",
	"image":  "1 image",
	"edit":   "1 image (the edit path bills the same card)",
	"speech": "N characters of synthesis",
	"video":  "5 seconds of video — the most expensive step here",
	"voice":  "1 clone enrollment (INVENTORY: it occupies a slot until deleted)",
}

// --- the client -----------------------------------------------------------

type client struct {
	http      *http.Client
	priv      ed25519.PrivateKey
	installID string
}

type storedIdentity struct {
	PrivateKey string `json:"privateKey"` // base64, this file is a credential — keep it local
	InstallID  string `json:"installId"`
}

func newClient() *client {
	c := &client{http: &http.Client{Timeout: 3 * time.Minute}}

	if raw, err := os.ReadFile(*identity); err == nil { // #nosec G304 — operator-supplied path
		var st storedIdentity
		if json.Unmarshal(raw, &st) == nil {
			key, kerr := base64.StdEncoding.DecodeString(st.PrivateKey)
			if kerr == nil && len(key) == ed25519.PrivateKeySize {
				c.priv = key
				c.installID = st.InstallID
				return c
			}
		}
		fmt.Fprintf(os.Stderr, "warning: %s unreadable, minting a new identity\n", *identity)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fail("generate key: %v", err)
	}
	c.priv = priv
	c.installID = c.register()

	blob, _ := json.MarshalIndent(storedIdentity{
		PrivateKey: base64.StdEncoding.EncodeToString(priv),
		InstallID:  c.installID,
	}, "", "  ")
	if err := os.WriteFile(*identity, blob, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist identity: %v\n", err)
	}
	return c
}

func (c *client) register() string {
	var out struct {
		InstallID string `json:"installId"`
	}
	c.do(http.MethodPost, "/v1/install",
		map[string]string{"fingerprint": "smoke", "client": "smoke"}, &out)
	if out.InstallID == "" {
		fail("register: gateway returned no installId")
	}
	return out.InstallID
}

type quotaView struct {
	Limit     int64 `json:"limit"`
	Used      int64 `json:"used"`
	Remaining int64 `json:"remaining"`
	Available bool  `json:"available"`
}

func (c *client) quota() quotaView {
	var q quotaView
	c.do(http.MethodGet, "/v1/quota", nil, &q)
	return q
}

// --- the steps ------------------------------------------------------------

func (c *client) chat() error {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	c.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "anselm-auto",
		"messages": []map[string]string{{"role": "user", "content": "Reply with exactly: pong"}},
		"stream":   false,
	}, &out)
	if len(out.Choices) == 0 {
		return fmt.Errorf("no choices returned")
	}
	fmt.Printf("           reply=%q tokens=%d\n",
		trim(out.Choices[0].Message.Content, 40), out.Usage.TotalTokens)
	return nil
}

func (c *client) image() (string, error) {
	var out struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	c.do(http.MethodPost, "/v1/images/generations", map[string]any{
		"prompt": "a single red maple leaf on wet slate, overhead, soft daylight",
		"size":   "1024x1024",
	}, &out)
	if len(out.Data) == 0 || out.Data[0].URL == "" {
		return "", fmt.Errorf("no artifact URL returned")
	}
	fmt.Printf("           url=%s\n", trim(out.Data[0].URL, 78))
	return out.Data[0].URL, nil
}

func (c *client) edit(srcURL string) error {
	if srcURL == "" {
		return fmt.Errorf("no source image: run the image step too, or pass -image <url>")
	}
	dataURL, err := c.asDataURL(srcURL)
	if err != nil {
		return err
	}
	var out struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	c.do(http.MethodPost, "/v1/images/edits", map[string]any{
		"prompt": "make the leaf golden yellow, keep everything else identical",
		"size":   "1024x1024",
		"image":  dataURL,
	}, &out)
	if len(out.Data) == 0 || out.Data[0].URL == "" {
		return fmt.Errorf("no artifact URL returned")
	}
	fmt.Printf("           url=%s\n", trim(out.Data[0].URL, 78))
	return nil
}

func (c *client) speech() error {
	body := c.raw(http.MethodPost, "/v1/audio/speech", map[string]any{
		"input": "网关冒烟测试，一二三。",
	})
	// The response is raw wav bytes, not JSON — see api.md §1.6.
	if len(body) < 44 || !bytes.HasPrefix(body, []byte("RIFF")) {
		return fmt.Errorf("expected RIFF wav bytes, got %d bytes starting %q", len(body), trim(string(body), 16))
	}
	out := "smoke-speech.wav"
	if err := os.WriteFile(out, body, 0o600); err != nil {
		return err
	}
	fmt.Printf("           wrote %s (%d bytes, RIFF header present)\n", out, len(body))
	return nil
}

func (c *client) video() error {
	var sub struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	c.do(http.MethodPost, "/v1/videos/generations", map[string]any{
		"prompt":  "a paper boat drifting down a rain gutter, close up",
		"seconds": 5,
	}, &sub)
	if sub.ID == "" {
		return fmt.Errorf("no handle returned")
	}
	fmt.Printf("           handle=%s status=%s — money is already settled at submit\n",
		trim(sub.ID, 46), sub.Status)

	deadline := time.Now().Add(*pollFor)
	for {
		var st struct {
			Status string `json:"status"`
			URL    string `json:"url"`
		}
		c.do(http.MethodGet, "/v1/videos/"+sub.ID, nil, &st)
		switch st.Status {
		case "succeeded":
			fmt.Printf("           url=%s\n", trim(st.URL, 78))
			return nil
		case "failed":
			return fmt.Errorf("upstream reported failed (still paid for — see GW-INV-56)")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still %s after %s (paid at submit; poll again later with the handle)",
				st.Status, *pollFor)
		}
		time.Sleep(10 * time.Second)
	}
}

func (c *client) voice() error {
	clip, err := os.ReadFile(*sample) // #nosec G304 — operator-supplied path
	if err != nil {
		return err
	}
	mime := "audio/wav"
	if strings.HasSuffix(strings.ToLower(*sample), ".mp3") {
		mime = "audio/mpeg"
	}
	sum := sha256.Sum256(clip)

	var up struct {
		UploadID      string `json:"uploadId"`
		ChunkMaxBytes int64  `json:"chunkMaxBytes"`
	}
	c.do(http.MethodPost, "/v1/media/uploads", map[string]any{
		"sha256": hex.EncodeToString(sum[:]), "mimeType": mime, "totalBytes": len(clip),
	}, &up)

	chunk := int(up.ChunkMaxBytes)
	if chunk <= 0 || chunk > len(clip) {
		chunk = len(clip)
	}
	for off := 0; off < len(clip); off += chunk {
		end := off + chunk
		if end > len(clip) {
			end = len(clip)
		}
		c.putBytes("/v1/media/uploads/"+up.UploadID, int64(off), clip[off:end])
	}

	var done struct {
		LeaseID string `json:"leaseId"`
	}
	c.do(http.MethodPost, "/v1/media/uploads/"+up.UploadID+"/complete", map[string]any{}, &done)
	if done.LeaseID == "" {
		return fmt.Errorf("complete returned no leaseId")
	}
	fmt.Printf("           uploaded %d bytes → lease=%s\n", len(clip), trim(done.LeaseID, 30))

	name := *voiceName
	if name == "" {
		name = "smoke-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	var v struct {
		VoiceID string `json:"voiceId"`
		Name    string `json:"name"`
	}
	c.do(http.MethodPost, "/v1/voices", map[string]any{"name": name, "leaseId": done.LeaseID}, &v)
	fmt.Printf("           voiceId=%s name=%q\n", trim(v.VoiceID, 30), v.Name)
	fmt.Printf("           NOTE: this occupies an inventory slot until you POST /v1/voices:delete\n")
	return nil
}

// --- wire plumbing --------------------------------------------------------

// do sends a signed request and decodes a JSON reply. Any non-2xx aborts the
// run with the gateway's own error envelope, because a smoke test that swallows
// a refusal is worse than no smoke test.
//
// 任何非 2xx 都直接中止,并打印网关自己的错误信封——一个把拒绝吞掉的冒烟测试比没有更糟。
func (c *client) do(method, path string, body any, out any) {
	raw := c.raw(method, path, body)
	if out == nil {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		fail("%s %s: decode: %v\nbody: %s", method, path, err, trim(string(raw), 300))
	}
}

func (c *client) raw(method, path string, body any) []byte {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req := c.sign(method, path, payload, method != http.MethodGet)
	resp, err := c.http.Do(req)
	if err != nil {
		fail("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fail("%s %s: HTTP %d\n  %s", method, path, resp.StatusCode, trim(string(raw), 400))
	}
	return raw
}

func (c *client) putBytes(path string, offset int64, chunk []byte) {
	req := c.sign(http.MethodPut, path, chunk, false)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(headerOffset, strconv.FormatInt(offset, 10))
	resp, err := c.http.Do(req)
	if err != nil {
		fail("PUT %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fail("PUT %s @%d: HTTP %d\n  %s", path, offset, resp.StatusCode, trim(string(raw), 300))
	}
}

// sign builds the proof: a fresh server nonce, then Ed25519 over a payload that
// binds method, target and the EXACT body hash. Replaying it elsewhere fails on
// every one of those three.
//
// sign 造 proof:先取一个新鲜的服务端 nonce,再对一份绑定了方法、目标与**精确 body 哈希**的载荷
// 做 Ed25519 签名。拿它去别处重放,这三项每一项都会挡住它。
func (c *client) sign(method, path string, body []byte, jsonBody bool) *http.Request {
	req, err := http.NewRequest(method, *base+path, bytes.NewReader(body))
	if err != nil {
		fail("build %s %s: %v", method, path, err)
	}
	if jsonBody {
		req.Header.Set("Content-Type", "application/json")
	}
	pub := c.priv.Public().(ed25519.PublicKey)

	kid := c.installID
	if path == "/v1/install" {
		thumb := sha256.Sum256(pub)
		kid = base64.RawURLEncoding.EncodeToString(thumb[:])
		req.Header.Set(headerPublicKey, base64.RawURLEncoding.EncodeToString(pub))
	} else {
		req.Header.Set(headerInstallID, c.installID)
	}

	nonce := c.nonce()
	bh := sha256.Sum256(body)
	jti := make([]byte, 16)
	_, _ = rand.Read(jti)
	target := strings.ToLower(req.URL.Host) + req.URL.EscapedPath()
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	payload, _ := json.Marshal(map[string]any{
		"v": 1, "kid": kid, "iat": time.Now().Unix(),
		"jti": base64.RawURLEncoding.EncodeToString(jti), "nonce": nonce,
		"htm": method, "htu": target,
		"bh": base64.RawURLEncoding.EncodeToString(bh[:]),
	})
	enc := base64.RawURLEncoding.EncodeToString(payload)
	req.Header.Set(headerProof, enc+"."+
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(c.priv, []byte(enc))))
	return req
}

func (c *client) nonce() string {
	resp, err := c.http.Get(*base + "/v1/proof/challenge")
	if err != nil {
		fail("challenge: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Nonce string `json:"nonce"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Nonce == "" {
		fail("challenge: no nonce (HTTP %d)", resp.StatusCode)
	}
	return out.Nonce
}

// asDataURL fetches an artifact and re-encodes it as a data URL, because the
// edit and animate routes refuse anything carrying a scheme — that refusal IS
// the SSRF mitigation (api.md §1.4), so the client does the fetching.
//
// asDataURL 取回产物并重编码成 data URL:改图与图生视频拒绝一切带 scheme 的东西,而那道拒绝
// **就是** SSRF 缓解本身(api.md §1.4),故由客户端来取。
func (c *client) asDataURL(u string) (string, error) {
	if strings.HasPrefix(u, "data:") {
		return u, nil
	}
	resp, err := c.http.Get(u) // #nosec G107 — an artifact URL this gateway just relayed to us
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	mime := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "smoke: "+format+"\n", a...)
	os.Exit(1)
}
