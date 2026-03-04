package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/joho/godotenv"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite"
)

// ─── Globals ──────────────────────────────────────────────────────────────────

var (
	waClient  *whatsmeow.Client
	qrMu      sync.Mutex
	lastQR    string
	connected bool
)

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	_ = godotenv.Load()

	token := os.Getenv("WA_GATEWAY_TOKEN")
	if token == "" {
		fmt.Println("ERROR: WA_GATEWAY_TOKEN tidak diset di .env!")
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./data/whatsapp.db"
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		fmt.Printf("ERROR membuat folder data: %v\n", err)
		os.Exit(1)
	}

	if err := setupWhatsApp(dbPath); err != nil {
		fmt.Printf("ERROR setup WhatsApp: %v\n", err)
		os.Exit(1)
	}

	r := mux.NewRouter()
	r.Use(corsMiddleware)
	r.Use(authMiddleware(token))

	r.HandleFunc("/health", handleHealth).Methods("GET", "OPTIONS")
	r.HandleFunc("/status", handleStatus).Methods("GET", "OPTIONS")
	r.HandleFunc("/qr", handleQR).Methods("GET", "OPTIONS")
	r.HandleFunc("/send", handleSend).Methods("POST", "OPTIONS")
	r.HandleFunc("/send-media", handleSendMedia).Methods("POST", "OPTIONS")
	r.HandleFunc("/send-base64", handleSendBase64).Methods("POST", "OPTIONS")

	fmt.Printf("✅ WA Gateway berjalan di http://localhost:%s\n", port)
	fmt.Printf("📱 Buka http://localhost:%s/qr untuk scan QR WhatsApp\n", port)

	if err := http.ListenAndServe(":"+port, r); err != nil {
		fmt.Printf("ERROR server: %v\n", err)
		os.Exit(1)
	}
}

// ─── WhatsApp Setup ───────────────────────────────────────────────────────────

func setupWhatsApp(dbPath string) error {
	logger := waLog.Stdout("WA-Gateway", "INFO", true)

	container, err := sqlstore.New(context.Background(), "sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", logger)
	if err != nil {
		return fmt.Errorf("gagal buka database: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		return fmt.Errorf("gagal ambil device: %w", err)
	}

	waClient = whatsmeow.NewClient(deviceStore, logger)
	waClient.AddEventHandler(handleWAEvent)

	if waClient.Store.ID == nil {
		// Belum pernah login → mulai QR flow
		ch, err := waClient.GetQRChannel(context.Background())
		if err != nil {
			return fmt.Errorf("gagal buat QR channel: %w", err)
		}

		if err := waClient.Connect(); err != nil {
			return fmt.Errorf("gagal connect: %w", err)
		}

		go func() {
			for item := range ch {
				if item.Event == "code" {
					qrMu.Lock()
					lastQR = item.Code
					qrMu.Unlock()
					fmt.Println("📲 QR baru tersedia — buka /qr di browser")
				} else if item.Event == "success" {
					connected = true
					fmt.Println("✅ WhatsApp berhasil terhubung!")
				} else {
					fmt.Printf("QR event: %s\n", item.Event)
				}
			}
		}()
	} else {
		// Sudah login — langsung connect
		if err := waClient.Connect(); err != nil {
			return fmt.Errorf("gagal reconnect: %w", err)
		}
		connected = true
		fmt.Println("✅ Session WhatsApp ditemukan, langsung terhubung!")
	}

	return nil
}

func handleWAEvent(evt interface{}) {
	switch evt.(type) {
	case *events.Connected:
		connected = true
		fmt.Println("✅ WhatsApp Connected")
	case *events.Disconnected:
		connected = false
		fmt.Println("⚠️  WhatsApp Disconnected")
	}
}

// ─── Middleware ───────────────────────────────────────────────────────────────

func authMiddleware(token string) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// /health, /qr, /status tidak perlu auth (bisa dibuka di browser langsung)
			noAuth := map[string]bool{"/health": true, "/qr": true, "/status": true}
			if noAuth[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			authHeader := r.Header.Get("Authorization")
			provided := strings.TrimPrefix(authHeader, "Bearer ")
			if provided == "" {
				provided = authHeader
			}
			if provided != token {
				jsonError(w, "Unauthorized: token tidak valid", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── HTTP Handlers ────────────────────────────────────────────────────────────

// GET /health
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	jsonOK(w, map[string]string{"status": "ok", "time": time.Now().Format(time.RFC3339)})
}

// GET /status
func handleStatus(w http.ResponseWriter, _ *http.Request) {
	jid := ""
	if waClient != nil && waClient.Store.ID != nil {
		jid = waClient.Store.ID.String()
	}
	jsonOK(w, map[string]interface{}{
		"connected": connected,
		"jid":       jid,
	})
}

// GET /qr — Tampilkan QR sebagai HTML (auto-refresh 20 detik)
func handleQR(w http.ResponseWriter, _ *http.Request) {
	if connected {
		jsonOK(w, map[string]string{"message": "WhatsApp sudah terhubung, tidak perlu scan QR."})
		return
	}

	qrMu.Lock()
	code := lastQR
	qrMu.Unlock()

	if code == "" {
		jsonError(w, "QR belum tersedia. Tunggu 5 detik lalu refresh.", http.StatusServiceUnavailable)
		return
	}

	png, err := qrcode.Encode(code, qrcode.Medium, 300)
	if err != nil {
		jsonError(w, "Gagal generate QR: "+err.Error(), http.StatusInternalServerError)
		return
	}

	b64 := base64.StdEncoding.EncodeToString(png)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
  <title>WA Gateway — Scan QR</title>
  <meta http-equiv="refresh" content="20">
  <style>
    body{font-family:sans-serif;background:#0f172a;color:#e2e8f0;display:flex;flex-direction:column;align-items:center;justify-content:center;height:100vh;margin:0}
    h2{color:#25D366;margin-bottom:16px}
    img{border-radius:16px;box-shadow:0 0 40px rgba(37,211,102,0.3);background:white;padding:16px}
    p{color:#94a3b8;font-size:14px;margin-top:12px;text-align:center}
    .badge{background:#1e293b;border:1px solid #334155;padding:6px 14px;border-radius:8px;font-size:12px;margin-top:8px}
  </style>
</head>
<body>
  <h2>📱 WA Gateway — Scan QR</h2>
  <img src="data:image/png;base64,%s" width="300" height="300" alt="QR Code WhatsApp">
  <p>Buka WhatsApp &rarr; <strong>Perangkat Tertaut</strong> &rarr; <strong>Tautkan Perangkat</strong> &rarr; Scan</p>
  <p class="badge">⏱ Auto-refresh setiap 20 detik</p>
</body>
</html>`, b64)
}

// POST /send — Kirim pesan teks
// Form: target=628xxx&message=Halo
func handleSend(w http.ResponseWriter, r *http.Request) {
	if !connected {
		jsonError(w, "WhatsApp belum terhubung. Scan QR di /qr dulu.", http.StatusServiceUnavailable)
		return
	}

	target, message, err := parseBody(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	jid, err := parseJID(target)
	if err != nil {
		jsonError(w, "Nomor tidak valid: "+err.Error(), http.StatusBadRequest)
		return
	}

	_, err = waClient.SendMessage(context.Background(), jid, &waE2E.Message{
		Conversation: proto.String(message),
	})
	if err != nil {
		jsonError(w, "Gagal kirim pesan: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"status": "sent", "target": target})
}

// POST /send-media — Kirim pesan + gambar dari URL
// Form: target=628xxx&message=Caption&url=https://...
func handleSendMedia(w http.ResponseWriter, r *http.Request) {
	if !connected {
		jsonError(w, "WhatsApp belum terhubung. Scan QR di /qr dulu.", http.StatusServiceUnavailable)
		return
	}

	target, message, err := parseBody(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	mediaURL := r.FormValue("url")
	jid, err := parseJID(target)
	if err != nil {
		jsonError(w, "Nomor tidak valid: "+err.Error(), http.StatusBadRequest)
		return
	}

	if mediaURL == "" {
		// Tidak ada URL, kirim teks saja
		waClient.SendMessage(context.Background(), jid, &waE2E.Message{
			Conversation: proto.String(message),
		})
		jsonOK(w, map[string]string{"status": "sent_text_only", "target": target})
		return
	}

	// Download gambar
	imgBytes, mime, err := downloadImage(mediaURL)
	if err != nil {
		// Fallback teks
		waClient.SendMessage(context.Background(), jid, &waE2E.Message{
			Conversation: proto.String(message + "\n\n[Foto: " + mediaURL + "]"),
		})
		jsonOK(w, map[string]string{"status": "sent_text_fallback", "reason": err.Error()})
		return
	}

	// Upload ke server WA
	uploaded, err := waClient.Upload(context.Background(), imgBytes, whatsmeow.MediaImage)
	if err != nil {
		jsonError(w, "Gagal upload media: "+err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = waClient.SendMessage(context.Background(), jid, &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			Caption:       proto.String(message),
			Mimetype:      proto.String(mime),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(imgBytes))),
		},
	})
	if err != nil {
		jsonError(w, "Gagal kirim media: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"status": "sent", "target": target, "type": "image"})
}

// POST /send-base64 — Kirim pesan + gambar dari base64 (bypass Firebase Storage)
// Form / JSON: target=628xxx&message=Caption&base64=data:image/jpeg;base64,...
func handleSendBase64(w http.ResponseWriter, r *http.Request) {
	if !connected {
		jsonError(w, "WhatsApp belum terhubung. Scan QR di /qr dulu.", http.StatusServiceUnavailable)
		return
	}

	r.ParseMultipartForm(32 << 20) // max 32MB
	r.ParseForm()

	target := r.FormValue("target")
	message := r.FormValue("message")
	b64str := r.FormValue("base64")

	// Coba decode dari JSON juga
	if target == "" || b64str == "" {
		var body struct {
			Target  string `json:"target"`
			Message string `json:"message"`
			Base64  string `json:"base64"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if target == "" {
				target = body.Target
			}
			if message == "" {
				message = body.Message
			}
			if b64str == "" {
				b64str = body.Base64
			}
		}
	}

	if target == "" {
		jsonError(w, "field 'target' wajib diisi", http.StatusBadRequest)
		return
	}
	if b64str == "" {
		jsonError(w, "field 'base64' wajib diisi", http.StatusBadRequest)
		return
	}

	// Hapus prefix data URL jika ada (misal: data:image/jpeg;base64,)
	mime := "image/jpeg"
	if idx := strings.Index(b64str, ","); idx != -1 {
		header := b64str[:idx]
		if strings.Contains(header, "image/png") {
			mime = "image/png"
		}
		b64str = b64str[idx+1:]
	}

	// Decode base64 ke bytes
	imgBytes, err := base64.StdEncoding.DecodeString(b64str)
	if err != nil {
		// Coba RawStdEncoding (tanpa padding)
		imgBytes, err = base64.RawStdEncoding.DecodeString(b64str)
		if err != nil {
			jsonError(w, "Gagal decode base64: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	jid, err := parseJID(target)
	if err != nil {
		jsonError(w, "Nomor tidak valid: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Upload ke server WhatsApp
	uploaded, err := waClient.Upload(context.Background(), imgBytes, whatsmeow.MediaImage)
	if err != nil {
		// Fallback: kirim teks saja jika upload gagal
		waClient.SendMessage(context.Background(), jid, &waE2E.Message{
			Conversation: proto.String(message),
		})
		jsonOK(w, map[string]string{"status": "sent_text_fallback", "reason": err.Error()})
		return
	}

	_, err = waClient.SendMessage(context.Background(), jid, &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			Caption:       proto.String(message),
			Mimetype:      proto.String(mime),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(imgBytes))),
		},
	})
	if err != nil {
		jsonError(w, "Gagal kirim gambar: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{"status": "sent", "target": target, "type": "image_base64"})
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func parseBody(r *http.Request) (target, message string, err error) {
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var body struct {
			Target  string `json:"target"`
			Message string `json:"message"`
		}
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			return "", "", fmt.Errorf("JSON tidak valid: %v", e)
		}
		target, message = body.Target, body.Message
	} else {
		r.ParseForm()
		target = r.FormValue("target")
		message = r.FormValue("message")
	}
	if target == "" {
		return "", "", fmt.Errorf("field 'target' wajib diisi")
	}
	if message == "" {
		return "", "", fmt.Errorf("field 'message' wajib diisi")
	}
	return target, message, nil
}

func parseJID(phone string) (types.JID, error) {
	phone = strings.TrimPrefix(phone, "+")
	phone = strings.ReplaceAll(phone, " ", "")
	phone = strings.ReplaceAll(phone, "-", "")
	if !strings.Contains(phone, "@") {
		phone = phone + "@s.whatsapp.net"
	}
	jid, err := types.ParseJID(phone)
	if err != nil {
		return types.JID{}, fmt.Errorf("format nomor tidak valid: %s", phone)
	}
	return jid, nil
}

func downloadImage(url string) ([]byte, string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d dari URL gambar", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = "image/jpeg"
	}
	return data, mime, nil
}

func jsonOK(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"data":    data,
	})
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   msg,
	})
}
