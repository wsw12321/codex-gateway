package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

// Login-page reads are unauthenticated, so they are served from a short-lived
// snapshot. Writes on this instance replace it immediately; other instances
// converge within siteCacheTTL.
const (
	siteCacheTTL         = 30 * time.Second
	siteImageUploadLimit = 5 << 20
	siteImageMaxPixels   = 12_000_000
)

var errInvalidSiteImage = errors.New("invalid site image")

type siteRepository interface {
	SiteSettings(context.Context) (store.SiteSettings, error)
	SiteImage(context.Context, string) (store.SiteImage, error)
	UpdateSiteSettings(context.Context, store.UpdateSiteSettingsParams) (store.SiteSettings, error)
	PutSiteImage(context.Context, store.SiteImage, store.SiteActor) (store.SiteSettings, error)
	DeleteSiteImage(context.Context, string, store.SiteActor) (store.SiteSettings, error)
}

type siteCache struct {
	mu       sync.Mutex
	settings store.SiteSettings
	loadedAt time.Time
	images   map[string]store.SiteImage
}

type siteSettingsFieldsDTO struct {
	CommunityEnabled bool   `json:"community_enabled"`
	CommunityTitle   string `json:"community_title"`
	CommunityQQGroup string `json:"community_qq_group"`
	CommunityJoinURL string `json:"community_join_url"`
	CommunityNote    string `json:"community_note"`
	RechargeEnabled  bool   `json:"recharge_enabled"`
	RechargeContact  string `json:"recharge_contact"`
	RechargeNote     string `json:"recharge_note"`
}

type siteImageDTO struct {
	URL       string    `json:"url"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	UpdatedAt time.Time `json:"updated_at"`
}

type siteCommunityDTO struct {
	Title   string        `json:"title"`
	QQGroup string        `json:"qq_group"`
	JoinURL string        `json:"join_url"`
	Note    string        `json:"note"`
	QRImage *siteImageDTO `json:"qr_image"`
}

type siteRechargeMethodDTO struct {
	Slot  string        `json:"slot"`
	Label string        `json:"label"`
	Image *siteImageDTO `json:"image"`
}

type siteRechargeDTO struct {
	Contact string                  `json:"contact"`
	Note    string                  `json:"note"`
	Methods []siteRechargeMethodDTO `json:"methods"`
}

var siteRechargeLabels = []struct{ slot, label string }{
	{store.SiteImageRechargeWeChat, "微信支付"},
	{store.SiteImageRechargeAlipay, "支付宝"},
}

func (s *Server) siteRoutes() {
	read := func(h http.HandlerFunc) http.Handler { return s.requireSession(s.ownerOnly(h)) }
	write := func(h http.HandlerFunc) http.Handler {
		return s.browserOrigin(s.requireRecentVerification(s.ownerOnly(h)))
	}
	s.mux.HandleFunc("GET /site/community", s.publicSiteCommunity)
	s.mux.HandleFunc("GET /site/images/"+store.SiteImageCommunityQR, s.publicSiteCommunityImage)
	s.mux.Handle("GET /admin/site", s.requireSession(http.HandlerFunc(s.siteContentJSON)))
	s.mux.Handle("GET /admin/site/images/{slot}", s.requireSession(http.HandlerFunc(s.sessionSiteImage)))
	s.mux.Handle("GET /admin/site-settings", read(s.siteSettingsJSON))
	s.mux.Handle("PUT /admin/site-settings", write(s.updateSiteSettings))
	s.mux.Handle("PUT /admin/site-settings/images/{slot}", write(s.putSiteImage))
	s.mux.Handle("DELETE /admin/site-settings/images/{slot}", write(s.deleteSiteImage))
}

func (s *Server) siteStorage() siteRepository {
	if s.siteRepo != nil {
		return s.siteRepo
	}
	return s.store
}

func siteActor(r *http.Request) store.SiteActor {
	return store.SiteActor{UserID: userFrom(r.Context()).ID, SessionID: sessionFrom(r.Context()).ID, SourceIP: safeIP(r.Context()), RequestID: httpx.RequestID(r.Context())}
}

func (s *Server) siteSnapshot(ctx context.Context) (store.SiteSettings, error) {
	s.site.mu.Lock()
	settings, fresh := s.site.settings, !s.site.loadedAt.IsZero() && time.Since(s.site.loadedAt) < siteCacheTTL
	s.site.mu.Unlock()
	if fresh {
		return settings, nil
	}
	settings, err := s.siteStorage().SiteSettings(ctx)
	if err != nil {
		return store.SiteSettings{}, err
	}
	s.rememberSite(settings)
	return settings, nil
}

// rememberSite never replaces a newer snapshot with a slower, older read.
func (s *Server) rememberSite(settings store.SiteSettings) {
	s.site.mu.Lock()
	defer s.site.mu.Unlock()
	if settings.Revision >= s.site.settings.Revision {
		s.site.settings, s.site.loadedAt = settings, time.Now()
	}
}

// Image bytes are cached by content hash, so a cached copy is never stale.
func (s *Server) siteImageData(ctx context.Context, info store.SiteImageInfo) (store.SiteImage, error) {
	s.site.mu.Lock()
	cached, ok := s.site.images[info.Slot]
	s.site.mu.Unlock()
	if ok && cached.SHA256 == info.SHA256 {
		return cached, nil
	}
	img, err := s.siteStorage().SiteImage(ctx, info.Slot)
	if err != nil {
		return store.SiteImage{}, err
	}
	s.rememberSiteImage(img)
	return img, nil
}

func (s *Server) rememberSiteImage(img store.SiteImage) {
	s.site.mu.Lock()
	defer s.site.mu.Unlock()
	if s.site.images == nil {
		s.site.images = make(map[string]store.SiteImage)
	}
	s.site.images[img.Slot] = img
}

func siteImageView(info store.SiteImageInfo, public bool) *siteImageDTO {
	path := "/admin/site/images/"
	if public {
		path = "/site/images/"
	}
	return &siteImageDTO{URL: path + info.Slot + "?v=" + info.SHA256[:16], Width: info.Width, Height: info.Height, UpdatedAt: info.UpdatedAt}
}

func siteCommunityView(settings store.SiteSettings) *siteCommunityDTO {
	if !settings.CommunityEnabled {
		return nil
	}
	view := &siteCommunityDTO{Title: settings.CommunityTitle, QQGroup: settings.CommunityQQGroup, JoinURL: settings.CommunityJoinURL, Note: settings.CommunityNote}
	if info, ok := settings.Images[store.SiteImageCommunityQR]; ok {
		view.QRImage = siteImageView(info, true)
	}
	return view
}

func siteRechargeView(settings store.SiteSettings) *siteRechargeDTO {
	if !settings.RechargeEnabled {
		return nil
	}
	view := &siteRechargeDTO{Contact: settings.RechargeContact, Note: settings.RechargeNote, Methods: []siteRechargeMethodDTO{}}
	for _, method := range siteRechargeLabels {
		if info, ok := settings.Images[method.slot]; ok {
			view.Methods = append(view.Methods, siteRechargeMethodDTO{Slot: method.slot, Label: method.label, Image: siteImageView(info, false)})
		}
	}
	return view
}

func siteSettingsResponse(settings store.SiteSettings) map[string]any {
	f := settings.SiteSettingsFields
	images := make(map[string]*siteImageDTO, len(settings.Images))
	for _, slot := range store.SiteImageSlots() {
		if info, ok := settings.Images[slot]; ok {
			images[slot] = siteImageView(info, false)
		}
	}
	return map[string]any{
		"revision": settings.Revision, "updated_at": settings.UpdatedAt, "images": images,
		"settings": siteSettingsFieldsDTO{
			CommunityEnabled: f.CommunityEnabled, CommunityTitle: f.CommunityTitle, CommunityQQGroup: f.CommunityQQGroup,
			CommunityJoinURL: f.CommunityJoinURL, CommunityNote: f.CommunityNote,
			RechargeEnabled: f.RechargeEnabled, RechargeContact: f.RechargeContact, RechargeNote: f.RechargeNote,
		},
	}
}

func (s *Server) publicSiteCommunity(w http.ResponseWriter, r *http.Request) {
	settings, err := s.siteSnapshot(r.Context())
	if err != nil {
		internalError(s, w, r, "read site community", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"community": siteCommunityView(settings)})
}

func (s *Server) siteContentJSON(w http.ResponseWriter, r *http.Request) {
	settings, err := s.siteSnapshot(r.Context())
	if err != nil {
		internalError(s, w, r, "read site content", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"community": siteCommunityView(settings), "recharge": siteRechargeView(settings)})
}

func (s *Server) publicSiteCommunityImage(w http.ResponseWriter, r *http.Request) {
	settings, err := s.siteSnapshot(r.Context())
	if err != nil {
		internalError(s, w, r, "read site community", err)
		return
	}
	info, ok := settings.Images[store.SiteImageCommunityQR]
	if !settings.CommunityEnabled || !ok {
		siteImageNotFound(w, r)
		return
	}
	s.serveSiteImage(w, r, info, "no-cache")
}

// Payment codes are shown only to signed-in users. The Owner can preview any
// slot, including one whose section is still hidden.
func (s *Server) sessionSiteImage(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	if !store.ValidSiteImageSlot(slot) {
		siteImageNotFound(w, r)
		return
	}
	settings, err := s.siteSnapshot(r.Context())
	if err != nil {
		internalError(s, w, r, "read site content", err)
		return
	}
	visible := settings.RechargeEnabled
	if slot == store.SiteImageCommunityQR {
		visible = settings.CommunityEnabled
	}
	info, ok := settings.Images[slot]
	if !ok || (!visible && userFrom(r.Context()).Role != store.UserRoleOwner) {
		siteImageNotFound(w, r)
		return
	}
	s.serveSiteImage(w, r, info, "private, no-cache")
}

func (s *Server) serveSiteImage(w http.ResponseWriter, r *http.Request, info store.SiteImageInfo, cacheControl string) {
	img, err := s.siteImageData(r.Context(), info)
	if errors.Is(err, store.ErrNotFound) {
		siteImageNotFound(w, r)
		return
	}
	if err != nil {
		internalError(s, w, r, "read site image", err)
		return
	}
	w.Header().Set("Content-Type", img.ContentType)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("ETag", `"`+img.SHA256+`"`)
	http.ServeContent(w, r, "", img.UpdatedAt, bytes.NewReader(img.Data))
}

func siteImageNotFound(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusNotFound, "invalid_request_error", "site_image_not_found", "图片不存在或未开放")
}

func (s *Server) siteSettingsJSON(w http.ResponseWriter, r *http.Request) {
	settings, err := s.siteStorage().SiteSettings(r.Context())
	if err != nil {
		internalError(s, w, r, "read site settings", err)
		return
	}
	s.rememberSite(settings)
	writeJSON(w, http.StatusOK, siteSettingsResponse(settings))
}

func siteText(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}

// The request replaces every text field; the dashboard always sends them all.
func (s *Server) updateSiteSettings(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Revision int64 `json:"revision"`
		siteSettingsFieldsDTO
	}
	if err := decodeJSON(w, r, &input, 16<<10); err != nil {
		badJSON(w, r, err)
		return
	}
	settings, err := s.siteStorage().UpdateSiteSettings(r.Context(), store.UpdateSiteSettingsParams{
		ExpectedRevision: input.Revision, Actor: siteActor(r),
		SiteSettingsFields: store.SiteSettingsFields{
			CommunityEnabled: input.CommunityEnabled, CommunityTitle: siteText(input.CommunityTitle),
			CommunityQQGroup: siteText(input.CommunityQQGroup), CommunityJoinURL: siteText(input.CommunityJoinURL),
			CommunityNote: siteText(input.CommunityNote), RechargeEnabled: input.RechargeEnabled,
			RechargeContact: siteText(input.RechargeContact), RechargeNote: siteText(input.RechargeNote),
		},
	})
	if err != nil {
		s.siteError(w, r, "update site settings", err)
		return
	}
	s.rememberSite(settings)
	writeJSON(w, http.StatusOK, siteSettingsResponse(settings))
}

func (s *Server) putSiteImage(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	if !store.ValidSiteImageSlot(slot) {
		siteImageNotFound(w, r)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "image/png" && mediaType != "image/jpeg") {
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, "invalid_request_error", "unsupported_site_image", "仅支持 PNG 或 JPEG 图片")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, siteImageUploadLimit))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, "invalid_request_error", "site_image_too_large", "图片不能超过 5 MiB")
		return
	}
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_site_image", "图片上传不完整，请重试")
		return
	}
	img, err := normalizeSiteImage(slot, mediaType, data)
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_site_image", "图片无法识别，或尺寸超过 4096×4096 像素")
		return
	}
	settings, err := s.siteStorage().PutSiteImage(r.Context(), img, siteActor(r))
	if err != nil {
		s.siteError(w, r, "save site image", err)
		return
	}
	s.rememberSiteImage(img)
	s.rememberSite(settings)
	writeJSON(w, http.StatusOK, siteSettingsResponse(settings))
}

func (s *Server) deleteSiteImage(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	if !store.ValidSiteImageSlot(slot) {
		siteImageNotFound(w, r)
		return
	}
	settings, err := s.siteStorage().DeleteSiteImage(r.Context(), slot, siteActor(r))
	if err != nil {
		s.siteError(w, r, "delete site image", err)
		return
	}
	s.rememberSite(settings)
	writeJSON(w, http.StatusOK, siteSettingsResponse(settings))
}

// normalizeSiteImage decodes and re-encodes an upload. Only pixels survive:
// EXIF (including location), comments, and appended payloads are dropped.
func normalizeSiteImage(slot, mediaType string, data []byte) (store.SiteImage, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != mediaType || config.Width < 1 || config.Height < 1 ||
		config.Width > store.SiteImageMaxDimension || config.Height > store.SiteImageMaxDimension || config.Width*config.Height > siteImageMaxPixels {
		return store.SiteImage{}, errInvalidSiteImage
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format {
		return store.SiteImage{}, errInvalidSiteImage
	}
	var out bytes.Buffer
	if format == "png" {
		err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, decoded)
	} else {
		err = jpeg.Encode(&out, decoded, &jpeg.Options{Quality: 92})
	}
	if err != nil || out.Len() > store.SiteImageMaxBytes {
		return store.SiteImage{}, errInvalidSiteImage
	}
	sum := sha256.Sum256(out.Bytes())
	bounds := decoded.Bounds()
	return store.SiteImage{
		SiteImageInfo: store.SiteImageInfo{Slot: slot, ContentType: mediaType, SHA256: hex.EncodeToString(sum[:]), Width: bounds.Dx(), Height: bounds.Dy()},
		Data:          out.Bytes(),
	}, nil
}

func (s *Server) siteError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	switch {
	case errors.Is(err, store.ErrSiteSettingsConflict):
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "site_settings_conflict", "站点设置已在其他页面修改，请刷新后重试")
	case errors.Is(err, store.ErrSiteCommunityContact):
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "site_community_contact_required", "开启交流群时，请至少填写 QQ 群号或加群链接")
	case errors.Is(err, store.ErrInvalid):
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_site_settings", "站点设置无效：请检查字数、群号（5–12 位数字）和链接（仅限 https://）")
	case errors.Is(err, store.ErrNotFound):
		siteImageNotFound(w, r)
	default:
		internalError(s, w, r, operation, err)
	}
}
