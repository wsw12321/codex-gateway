package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

type fakeSiteRepository struct {
	settings   store.SiteSettings
	images     map[string]store.SiteImage
	reads      int
	imageReads int
	updates    []store.UpdateSiteSettingsParams
	puts       []store.SiteImage
	deletes    []string
	actors     []store.SiteActor
	err        error
}

func newFakeSiteRepository() *fakeSiteRepository {
	return &fakeSiteRepository{settings: store.SiteSettings{Revision: 1, Images: map[string]store.SiteImageInfo{}}, images: map[string]store.SiteImage{}}
}

func (f *fakeSiteRepository) snapshot() store.SiteSettings {
	settings := f.settings
	settings.Images = make(map[string]store.SiteImageInfo, len(f.images))
	for slot, img := range f.images {
		settings.Images[slot] = img.SiteImageInfo
	}
	return settings
}

func (f *fakeSiteRepository) SiteSettings(context.Context) (store.SiteSettings, error) {
	f.reads++
	return f.snapshot(), f.err
}

func (f *fakeSiteRepository) SiteImage(_ context.Context, slot string) (store.SiteImage, error) {
	f.imageReads++
	img, ok := f.images[slot]
	if !ok {
		return store.SiteImage{}, store.ErrNotFound
	}
	return img, f.err
}

func (f *fakeSiteRepository) UpdateSiteSettings(_ context.Context, params store.UpdateSiteSettingsParams) (store.SiteSettings, error) {
	f.updates = append(f.updates, params)
	f.actors = append(f.actors, params.Actor)
	if f.err != nil {
		return store.SiteSettings{}, f.err
	}
	if err := store.ValidateSiteSettings(params.SiteSettingsFields); err != nil {
		return store.SiteSettings{}, err
	}
	if params.ExpectedRevision != f.settings.Revision {
		return store.SiteSettings{}, store.ErrSiteSettingsConflict
	}
	f.settings.SiteSettingsFields = params.SiteSettingsFields
	f.settings.Revision++
	return f.snapshot(), nil
}

func (f *fakeSiteRepository) PutSiteImage(_ context.Context, img store.SiteImage, actor store.SiteActor) (store.SiteSettings, error) {
	f.puts = append(f.puts, img)
	f.actors = append(f.actors, actor)
	if f.err != nil {
		return store.SiteSettings{}, f.err
	}
	f.images[img.Slot] = img
	f.settings.Revision++
	return f.snapshot(), nil
}

func (f *fakeSiteRepository) DeleteSiteImage(_ context.Context, slot string, actor store.SiteActor) (store.SiteSettings, error) {
	f.deletes = append(f.deletes, slot)
	f.actors = append(f.actors, actor)
	if _, ok := f.images[slot]; !ok {
		return store.SiteSettings{}, store.ErrNotFound
	}
	delete(f.images, slot)
	f.settings.Revision++
	return f.snapshot(), nil
}

func (f *fakeSiteRepository) addImage(t *testing.T, slot string) store.SiteImage {
	t.Helper()
	img, err := normalizeSiteImage(slot, "image/png", testPNG(t, 8, 8))
	if err != nil {
		t.Fatal(err)
	}
	img.UpdatedAt = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	f.images[slot] = img
	return img
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		img.Set(x, x%height, color.NRGBA{A: 255})
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func newSiteTestServer(t *testing.T, role string, verified *time.Time) (*Server, *fakeSiteRepository) {
	t.Helper()
	s, _ := newBillingSourceTestServer(t, role, verified)
	f := newFakeSiteRepository()
	s.siteRepo = f
	return s, f
}

func siteTestRequest(t *testing.T, method, path string, body []byte, session bool) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Origin", "https://gateway.example")
	r.Header.Set("Content-Type", "application/json")
	if session {
		addBillingSourceTestSession(t, r)
	}
	return r
}

func serveSite(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestSiteRoutesRequireSessionOwnerAndWriteVerification(t *testing.T) {
	settingsBody := []byte(`{"revision":1,"community_enabled":false,"community_title":"","community_qq_group":"","community_join_url":"","community_note":"","recharge_enabled":false,"recharge_contact":"","recharge_note":""}`)
	for _, tt := range []struct {
		name, method, path, role, origin, code string
		session, verified                      bool
		body                                   []byte
		status                                 int
	}{
		{name: "public community", method: "GET", path: "/site/community", status: 200},
		{name: "content anonymous", method: "GET", path: "/admin/site", status: 401, code: "session_required"},
		{name: "content member", method: "GET", path: "/admin/site", session: true, role: "member", status: 200},
		{name: "settings member", method: "GET", path: "/admin/site-settings", session: true, role: "member", status: 403, code: "owner_required"},
		{name: "settings owner", method: "GET", path: "/admin/site-settings", session: true, role: "owner", status: 200},
		{name: "write foreign origin", method: "PUT", path: "/admin/site-settings", session: true, role: "owner", verified: true, origin: "https://evil.example", body: settingsBody, status: 403, code: "invalid_origin"},
		{name: "write unverified", method: "PUT", path: "/admin/site-settings", session: true, role: "owner", origin: "https://gateway.example", body: settingsBody, status: 403, code: "recent_identity_verification_required"},
		{name: "write member", method: "PUT", path: "/admin/site-settings", session: true, role: "member", verified: true, origin: "https://gateway.example", body: settingsBody, status: 403, code: "owner_required"},
		{name: "image member", method: "PUT", path: "/admin/site-settings/images/recharge_wechat", session: true, role: "member", verified: true, origin: "https://gateway.example", body: []byte("x"), status: 403, code: "owner_required"},
		{name: "delete anonymous", method: "DELETE", path: "/admin/site-settings/images/recharge_wechat", origin: "https://gateway.example", status: 401, code: "session_required"},
		{name: "write owner", method: "PUT", path: "/admin/site-settings", session: true, role: "owner", verified: true, origin: "https://gateway.example", body: settingsBody, status: 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var verified *time.Time
			if tt.verified {
				now := time.Now()
				verified = &now
			}
			s, f := newSiteTestServer(t, tt.role, verified)
			r := siteTestRequest(t, tt.method, tt.path, tt.body, tt.session)
			r.Header.Set("Origin", tt.origin)
			w := serveSite(s, r)
			if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.code) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if tt.status != 200 && (len(f.updates) != 0 || len(f.puts) != 0 || len(f.deletes) != 0) {
				t.Fatal("unauthorized request reached site storage")
			}
		})
	}
}

func TestSiteContentHidesDisabledSectionsAndPaymentCodesFromAnonymousVisitors(t *testing.T) {
	s, f := newSiteTestServer(t, "member", nil)
	f.addImage(t, store.SiteImageCommunityQR)
	f.addImage(t, store.SiteImageRechargeWeChat)

	// Both sections start disabled: nothing is visible to anyone but the Owner.
	if w := serveSite(s, siteTestRequest(t, "GET", "/site/community", nil, false)); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"community":null}` {
		t.Fatalf("disabled public community status=%d body=%s", w.Code, w.Body)
	}
	if w := serveSite(s, siteTestRequest(t, "GET", "/site/images/community_qr", nil, false)); w.Code != 404 {
		t.Fatalf("disabled community QR status=%d", w.Code)
	}
	if w := serveSite(s, siteTestRequest(t, "GET", "/admin/site/images/recharge_wechat", nil, true)); w.Code != 404 {
		t.Fatalf("member saw disabled payment code: %d", w.Code)
	}

	f.settings.SiteSettingsFields = store.SiteSettingsFields{CommunityEnabled: true, CommunityQQGroup: "123456789", CommunityJoinURL: "https://qm.qq.com/q/abc", RechargeEnabled: true, RechargeContact: "群主"}
	s.site = siteCache{}
	w := serveSite(s, siteTestRequest(t, "GET", "/site/community", nil, false))
	var public map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &public); err != nil || len(public) != 1 || !strings.Contains(string(public["community"]), `"qq_group":"123456789"`) ||
		!strings.Contains(string(public["community"]), `"url":"/site/images/community_qr?v=`) {
		t.Fatalf("public community status=%d body=%s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "recharge") || strings.Contains(w.Body.String(), "群主") {
		t.Fatalf("public response leaked recharge content: %s", w.Body)
	}

	w = serveSite(s, siteTestRequest(t, "GET", "/admin/site", nil, true))
	var content struct {
		Community *siteCommunityDTO `json:"community"`
		Recharge  *siteRechargeDTO  `json:"recharge"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &content); err != nil || content.Community == nil || content.Recharge == nil || content.Recharge.Contact != "群主" ||
		len(content.Recharge.Methods) != 1 || content.Recharge.Methods[0].Label != "微信支付" || !strings.HasPrefix(content.Recharge.Methods[0].Image.URL, "/admin/site/images/recharge_wechat?v=") {
		t.Fatalf("member content status=%d body=%s", w.Code, w.Body)
	}

	// Payment codes are never served on the anonymous image route.
	if w := serveSite(s, siteTestRequest(t, "GET", "/site/images/recharge_wechat", nil, false)); w.Code == 200 && w.Header().Get("Content-Type") == "image/png" {
		t.Fatal("payment code is publicly served")
	}
	if w := serveSite(s, siteTestRequest(t, "GET", "/admin/site/images/recharge_wechat", nil, false)); w.Code != 401 {
		t.Fatalf("anonymous payment code status=%d", w.Code)
	}
}

func TestSiteImagesAreServedAsInertCacheValidatedContent(t *testing.T) {
	s, f := newSiteTestServer(t, "owner", nil)
	img := f.addImage(t, store.SiteImageCommunityQR)
	f.settings.CommunityEnabled, f.settings.CommunityQQGroup = true, "123456789"

	w := serveSite(s, siteTestRequest(t, "GET", "/site/images/community_qr", nil, false))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), img.Data) {
		t.Fatalf("status=%d", w.Code)
	}
	for header, want := range map[string]string{
		"Content-Type": "image/png", "X-Content-Type-Options": "nosniff", "Cache-Control": "no-cache", "ETag": `"` + img.SHA256 + `"`,
	} {
		if got := w.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("image response lacks the restrictive CSP")
	}
	conditional := siteTestRequest(t, "GET", "/site/images/community_qr", nil, false)
	conditional.Header.Set("If-None-Match", `"`+img.SHA256+`"`)
	if w := serveSite(s, conditional); w.Code != http.StatusNotModified {
		t.Fatalf("conditional status=%d", w.Code)
	}
	if f.reads != 1 || f.imageReads != 1 {
		t.Fatalf("public reads were not cached: settings=%d images=%d", f.reads, f.imageReads)
	}

	// The Owner previews hidden slots; members do not see them.
	f.addImage(t, store.SiteImageRechargeAlipay)
	s.site = siteCache{}
	if w := serveSite(s, siteTestRequest(t, "GET", "/admin/site/images/recharge_alipay", nil, true)); w.Code != 200 || w.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("owner preview status=%d cache=%q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := serveSite(s, siteTestRequest(t, "GET", "/admin/site/images/unknown", nil, true)); w.Code != 404 {
		t.Fatalf("unknown slot status=%d", w.Code)
	}
}

func TestSiteSettingsUpdateNormalizesAttributesAndRefreshesCache(t *testing.T) {
	now := time.Now()
	s, f := newSiteTestServer(t, "owner", &now)
	if w := serveSite(s, siteTestRequest(t, "GET", "/site/community", nil, false)); w.Code != 200 {
		t.Fatalf("prime status=%d", w.Code)
	}
	body := []byte(`{"revision":1,"community_enabled":true,"community_title":" 水源喵交流群 ","community_qq_group":" 123456789 ","community_join_url":"","community_note":"第一行\r\n第二行 ","recharge_enabled":true,"recharge_contact":"群主","recharge_note":""}`)
	w := serveSite(s, siteTestRequest(t, "PUT", "/admin/site-settings", body, true))
	if w.Code != 200 || len(f.updates) != 1 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	got := f.updates[0]
	if got.CommunityTitle != "水源喵交流群" || got.CommunityQQGroup != "123456789" || got.CommunityNote != "第一行\n第二行" || got.ExpectedRevision != 1 {
		t.Fatalf("normalized update=%+v", got)
	}
	if got.Actor.UserID != "self-1" || got.Actor.SessionID != "session-1" || got.Actor.RequestID == "" {
		t.Fatalf("actor=%+v", got.Actor)
	}
	// The cached public snapshot is replaced by the write, not after the TTL.
	if w := serveSite(s, siteTestRequest(t, "GET", "/site/community", nil, false)); !strings.Contains(w.Body.String(), "水源喵交流群") || f.reads != 1 {
		t.Fatalf("cache not refreshed reads=%d body=%s", f.reads, w.Body)
	}

	for _, tt := range []struct {
		name, body, code string
		status           int
	}{
		{"stale revision", string(body), "site_settings_conflict", 409},
		{"missing contact", `{"revision":2,"community_enabled":true,"community_title":"","community_qq_group":"","community_join_url":"","community_note":"","recharge_enabled":false,"recharge_contact":"","recharge_note":""}`, "site_community_contact_required", 400},
		{"script link", `{"revision":2,"community_enabled":true,"community_title":"","community_qq_group":"","community_join_url":"javascript:alert(1)","community_note":"","recharge_enabled":false,"recharge_contact":"","recharge_note":""}`, "invalid_site_settings", 400},
		{"unknown field", `{"revision":2,"announcement":"x"}`, "invalid_json", 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if w := serveSite(s, siteTestRequest(t, "PUT", "/admin/site-settings", []byte(tt.body), true)); w.Code != tt.status || !strings.Contains(w.Body.String(), tt.code) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
		})
	}
}

func TestSiteImageUploadRejectsUnsafeInputAndReencodes(t *testing.T) {
	now := time.Now()
	s, f := newSiteTestServer(t, "owner", &now)
	upload := func(contentType string, body []byte) *httptest.ResponseRecorder {
		r := siteTestRequest(t, "PUT", "/admin/site-settings/images/recharge_wechat", body, true)
		r.Header.Set("Content-Type", contentType)
		return serveSite(s, r)
	}
	valid := testPNG(t, 16, 16)
	for _, tt := range []struct {
		name, contentType, code string
		body                    []byte
		status                  int
	}{
		{"svg", "image/svg+xml", "unsupported_site_image", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), 415},
		{"html as png", "image/png", "invalid_site_image", []byte("<html><script>alert(1)</script></html>"), 400},
		{"png declared jpeg", "image/jpeg", "invalid_site_image", valid, 400},
		{"too wide", "image/png", "invalid_site_image", testPNG(t, 4097, 1), 400},
		{"too large", "image/png", "site_image_too_large", append(append([]byte{}, valid...), make([]byte, siteImageUploadLimit)...), 413},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if w := upload(tt.contentType, tt.body); w.Code != tt.status || !strings.Contains(w.Body.String(), tt.code) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
		})
	}
	if len(f.puts) != 0 {
		t.Fatal("rejected upload reached storage")
	}

	// Trailing payloads (polyglots) and metadata do not survive re-encoding.
	payload := append(append([]byte{}, valid...), []byte("<script>alert(1)</script>")...)
	if w := upload("image/png", payload); w.Code != 200 || len(f.puts) != 1 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	stored := f.puts[0]
	sum := sha256.Sum256(stored.Data)
	if bytes.Contains(stored.Data, []byte("script")) || stored.SHA256 != hex.EncodeToString(sum[:]) || stored.Width != 16 || stored.Height != 16 || stored.ContentType != "image/png" {
		t.Fatalf("stored image = %+v", stored.SiteImageInfo)
	}
	if _, err := png.Decode(bytes.NewReader(stored.Data)); err != nil {
		t.Fatal(err)
	}

	var photo bytes.Buffer
	if err := jpeg.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 4, 3)), nil); err != nil {
		t.Fatal(err)
	}
	// An APP1 (EXIF) segment directly after SOI must be dropped.
	exif := append([]byte{0xff, 0xd8, 0xff, 0xe1, 0x00, 0x12}, []byte("Exif\x00\x00GPS-SECRET")...)
	withEXIF := append(exif, photo.Bytes()[2:]...)
	if w := upload("image/jpeg", withEXIF); w.Code != 200 || len(f.puts) != 2 {
		t.Fatalf("jpeg status=%d body=%s", w.Code, w.Body)
	}
	if bytes.Contains(f.puts[1].Data, []byte("GPS-SECRET")) || f.puts[1].ContentType != "image/jpeg" {
		t.Fatal("JPEG metadata survived re-encoding")
	}

	// Deleting reports a missing image instead of succeeding silently.
	r := siteTestRequest(t, "DELETE", "/admin/site-settings/images/community_qr", nil, true)
	if w := serveSite(s, r); w.Code != 404 {
		t.Fatalf("delete missing status=%d", w.Code)
	}
}

func TestSiteDashboardShowsFixedRechargeGuidance(t *testing.T) {
	t.Parallel()
	html := string(indexHTML)
	for _, required := range []string{
		`id="auth-community"`, `id="overview-community"`, `id="billing-recharge"`, `data-section="site-settings"`, `href="#site-settings"`,
		"付款时务必在备注中填写你的用户名", "充值需要管理员确认后才会到账", "建议付款前先在交流群联系客服确认",
		`accept="image/png,image/jpeg"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("dashboard HTML is missing %s", required)
		}
	}
}

func TestSiteSettingsDashboardBehavior(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is required for dashboard behavior tests")
	}
	output, err := exec.Command("node", "--test", "testdata/site_settings_ui_test.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("site settings dashboard tests: %v\n%s", err, output)
	}
}
