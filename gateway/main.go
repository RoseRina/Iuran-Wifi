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

	// Statistik pengiriman pesan
	statsMu      sync.Mutex
	totalSent    int64
	totalFailed  int64
	lastSentAt   time.Time
	lastSentTo   string
	lastSentType string
	gatewayStart = time.Now()

	// Konfigurasi (disimpan ke data/config.json)
	cfgMu      sync.RWMutex
	gwConfig   GatewayConfig
	configPath string
)

// GatewayConfig menyimpan konfigurasi yang bisa diubah via admin panel
type GatewayConfig struct {
	RecipientNumber string `json:"recipient_number"`
	IuranAmount     int    `json:"iuran_amount"`
}

func loadConfig(path string) {
	configPath = path
	data, err := os.ReadFile(path)
	if err != nil {
		// File belum ada, pakai default
		gwConfig = GatewayConfig{RecipientNumber: "6282149335323", IuranAmount: 150000}
		saveConfig()
		return
	}
	if err := json.Unmarshal(data, &gwConfig); err != nil {
		gwConfig = GatewayConfig{RecipientNumber: "6282149335323", IuranAmount: 150000}
	}
}

func saveConfig() {
	data, _ := json.MarshalIndent(gwConfig, "", "  ")
	os.WriteFile(configPath, data, 0644)
}

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

	// Load konfigurasi dari file
	loadConfig(filepath.Join(filepath.Dir(dbPath), "config.json"))

	if err := setupWhatsApp(dbPath); err != nil {
		fmt.Printf("ERROR setup WhatsApp: %v\n", err)
		os.Exit(1)
	}

	r := mux.NewRouter()
	r.Use(corsMiddleware)
	r.Use(authMiddleware(token))

	r.HandleFunc("/", handleAdminPanel).Methods("GET")
	r.HandleFunc("/health", handleHealth).Methods("GET", "OPTIONS")
	r.HandleFunc("/status", handleStatus).Methods("GET", "OPTIONS")
	r.HandleFunc("/qr", handleQR).Methods("GET", "OPTIONS")
	r.HandleFunc("/config", handleConfig).Methods("GET", "POST", "OPTIONS")
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
			// Endpoint yang tidak perlu auth
			noAuth := map[string]bool{"/health": true, "/qr": true, "/status": true, "/": true}
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
	statsMu.Lock()
	lastAt := ""
	if !lastSentAt.IsZero() {
		lastAt = lastSentAt.Format("02 Jan 2006 15:04:05 WIB")
	}
	stats := map[string]interface{}{
		"total_sent":   totalSent,
		"total_failed": totalFailed,
		"last_sent_at": lastAt,
		"last_sent_to": lastSentTo,
		"last_type":    lastSentType,
	}
	statsMu.Unlock()
	jsonOK(w, map[string]interface{}{
		"connected": connected,
		"jid":       jid,
		"uptime":    time.Since(gatewayStart).Round(time.Second).String(),
		"stats":     stats,
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

// GET / — Halaman admin panel
func handleAdminPanel(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html>
<html lang="id">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>WA Gateway Admin</title>
  <style>
    :root{--bg:#0f172a;--surf:#1e293b;--bdr:#334155;--txt:#e2e8f0;--muted:#94a3b8;--green:#22c55e;--red:#ef4444;--acc:#f59e0b}
    *{box-sizing:border-box;margin:0;padding:0}
    body{font-family:system-ui,sans-serif;background:var(--bg);color:var(--txt);min-height:100vh;padding:20px}
    .wrap{max-width:580px;margin:0 auto}
    h1{font-size:1.3rem;font-weight:800;color:var(--acc);margin-bottom:20px;display:flex;align-items:center;gap:8px}
    .card{background:var(--surf);border:1px solid var(--bdr);border-radius:14px;padding:18px;margin-bottom:14px}
    .card h2{font-size:.7rem;font-weight:700;text-transform:uppercase;letter-spacing:.1em;color:var(--muted);margin-bottom:14px}
    .badge{display:inline-flex;align-items:center;gap:6px;padding:4px 12px;border-radius:20px;font-size:.75rem;font-weight:700}
    .bg{background:rgba(34,197,94,.1);color:var(--green);border:1px solid rgba(34,197,94,.2)}
    .br{background:rgba(239,68,68,.1);color:var(--red);border:1px solid rgba(239,68,68,.2)}
    .dot{width:8px;height:8px;border-radius:50%;background:currentColor}
    .dg{animation:pulse 2s infinite}
    @keyframes pulse{0%,100%{opacity:1}50%{opacity:.4}}
    .grid2{display:grid;grid-template-columns:1fr 1fr;gap:10px}
    .sbox{background:rgba(255,255,255,.03);border-radius:10px;padding:12px}
    .sl{font-size:.65rem;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);margin-bottom:4px}
    .sv{font-size:1.2rem;font-weight:800}
    .sv.g{color:var(--green)}.sv.r{color:var(--red)}
    .sm{font-size:.7rem;color:var(--muted);margin-top:2px}
    .row{display:flex;justify-content:space-between;align-items:center;margin:5px 0;font-size:.8rem}
    .row span:first-child{color:var(--muted)}
    .row span:last-child{font-weight:600;font-family:monospace;font-size:.75rem;max-width:60%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
    label{font-size:.72rem;color:var(--muted);font-weight:600;display:block;margin-top:10px}
    label:first-child{margin-top:0}
    input{width:100%;background:rgba(255,255,255,.05);border:1px solid var(--bdr);border-radius:10px;padding:9px 13px;color:var(--txt);font-size:.875rem;margin-top:5px}
    input:focus{outline:none;border-color:var(--acc)}
    button{width:100%;padding:11px;border-radius:10px;font-weight:700;font-size:.78rem;text-transform:uppercase;letter-spacing:.05em;border:none;cursor:pointer;margin-top:10px;transition:opacity .2s}
    button:hover{opacity:.8}
    .ba{background:linear-gradient(135deg,#f59e0b,#f97316);color:#0f172a}
    .bg2{background:rgba(34,197,94,.12);color:var(--green);border:1px solid rgba(34,197,94,.25)}
    .bgh{background:rgba(255,255,255,.05);color:var(--txt);border:1px solid var(--bdr)}
    .bred{background:rgba(239,68,68,.1);color:var(--red);border:1px solid rgba(239,68,68,.2)}
    hr{border:none;border-top:1px solid var(--bdr);margin:10px 0}
    #toast{position:fixed;bottom:20px;left:50%;transform:translateX(-50%);background:var(--surf);border:1px solid var(--bdr);padding:10px 20px;border-radius:10px;font-size:.8rem;display:none;z-index:999;white-space:nowrap}
    .hidden{display:none!important}
    #login-card{max-width:340px;margin:12vh auto}
  </style>
</head>
<body>
<div id="toast"></div>

<div id="login-card" class="card">
  <h2>🔐 WA Gateway Admin</h2>
  <label>Token</label>
  <input type="password" id="tok" placeholder="Masukkan token gateway..." onkeydown="if(event.key==='Enter')doLogin()">
  <button class="ba" onclick="doLogin()">Login</button>
</div>

<div id="dash" class="wrap hidden">
  <h1>📡 WA Gateway Admin</h1>

  <div class="card">
    <h2>Status Koneksi</h2>
    <div style="display:flex;align-items:center;gap:10px;margin-bottom:10px">
      <span class="badge br" id="cbadge"><span class="dot" id="cdot"></span><span id="ctxt">Memuat...</span></span>
      <button class="bgh" onclick="window.open('/qr','_blank')" style="width:auto;padding:4px 12px;font-size:.7rem;margin-top:0">📱 Scan QR</button>
    </div>
    <hr>
    <div class="row"><span>JID / Nomor</span><span id="jid">—</span></div>
    <div class="row"><span>Uptime</span><span id="upt">—</span></div>
  </div>

  <div class="card">
    <h2>📊 Statistik Pesan</h2>
    <div class="grid2">
      <div class="sbox"><div class="sl">Terkirim</div><div class="sv g" id="s-sent">—</div></div>
      <div class="sbox"><div class="sl">Gagal</div><div class="sv r" id="s-fail">—</div></div>
      <div class="sbox" style="grid-column:1/-1">
        <div class="sl">Terakhir Kirim</div>
        <div class="sv" style="font-size:.95rem" id="s-at">Belum ada</div>
        <div class="sm" id="s-to">—</div>
      </div>
    </div>
  </div>

  <div class="card">
    <h2>⚙️ Konfigurasi</h2>
    <label>Nomor WA Penerima (tanpa +, tanpa strip)</label>
    <input type="tel" id="cfg-num" placeholder="6282149335323">
    <label>Nominal Iuran (Rp)</label>
    <input type="number" id="cfg-amt" placeholder="150000">
    <button class="ba" onclick="saveConfig()">💾 Simpan Konfigurasi</button>
  </div>

  <div class="card">
    <h2>📤 Test Kirim WhatsApp</h2>
    <label>Nomor Tujuan</label>
    <input type="tel" id="t-num" placeholder="628xxxx (dari config jika kosong)">
    <label>Pesan</label>
    <input type="text" id="t-msg" value="Test dari WA Gateway Admin 🚀">
    <button class="bg2" onclick="sendTest()">Kirim Test</button>
  </div>

  <button class="bred" onclick="logout()" style="margin-bottom:24px">Logout</button>
</div>

<script>
  let T='';
  const API=location.origin;
  const toast=document.getElementById('toast');

  function showToast(m){toast.textContent=m;toast.style.display='block';setTimeout(()=>toast.style.display='none',3000)}

  function doLogin(){
    const t=document.getElementById('tok').value.trim();
    if(!t)return;
    T=t;localStorage.setItem('wa_token',t);init();
  }

  function logout(){localStorage.removeItem('wa_token');location.reload()}

  async function init(){
    try{
      const r=await fetch(API+'/status',{headers:{'Authorization':'Bearer '+T}});
      if(r.status===401){showToast('❌ Token salah!');return}
      document.getElementById('login-card').classList.add('hidden');
      document.getElementById('dash').classList.remove('hidden');
      loadCfg();tick();setInterval(tick,5000);
    }catch(e){showToast('Tidak bisa terhubung!')}
  }

  async function tick(){
    try{
      const r=await fetch(API+'/status',{headers:{'Authorization':'Bearer '+T}});
      const d=(await r.json()).data;
      const ok=d.connected;
      document.getElementById('cbadge').className='badge '+(ok?'bg':'br');
      document.getElementById('cdot').className='dot '+(ok?'dg':'');
      document.getElementById('ctxt').textContent=ok?'Terhubung ✅':'Terputus ❌';
      document.getElementById('jid').textContent=d.jid||'—';
      document.getElementById('upt').textContent=d.uptime||'—';
      if(d.stats){
        document.getElementById('s-sent').textContent=d.stats.total_sent??0;
        document.getElementById('s-fail').textContent=d.stats.total_failed??0;
        document.getElementById('s-at').textContent=d.stats.last_sent_at||'Belum ada';
        document.getElementById('s-to').textContent=d.stats.last_sent_to?'→ '+d.stats.last_sent_to+' ('+d.stats.last_type+')':'—';
      }
    }catch(e){}
  }

  async function loadCfg(){
    try{
      const r=await fetch(API+'/config',{headers:{'Authorization':'Bearer '+T}});
      const d=(await r.json()).data;
      if(d){document.getElementById('cfg-num').value=d.recipient_number||'';document.getElementById('cfg-amt').value=d.iuran_amount||'';}
    }catch(e){}
  }

  async function saveConfig(){
    const body={recipient_number:document.getElementById('cfg-num').value.trim(),iuran_amount:parseInt(document.getElementById('cfg-amt').value)||0};
    try{
      const r=await fetch(API+'/config',{method:'POST',headers:{'Authorization':'Bearer '+T,'Content-Type':'application/json'},body:JSON.stringify(body)});
      const j=await r.json();
      showToast(j.success?'✅ Tersimpan!':'❌ '+j.error);
    }catch(e){showToast('Error: '+e.message)}
  }

  async function sendTest(){
    const num=document.getElementById('t-num').value.trim()||document.getElementById('cfg-num').value.trim();
    const msg=document.getElementById('t-msg').value.trim()||'Test WA Gateway 🚀';
    if(!num){showToast('Masukkan nomor tujuan!');return}
    try{
      const r=await fetch(API+'/send',{method:'POST',headers:{'Authorization':'Bearer '+T,'Content-Type':'application/json'},body:JSON.stringify({target:num,message:msg})});
      const j=await r.json();
      showToast(j.success?'✅ Terkirim!':'❌ '+j.error);
    }catch(e){showToast('Error: '+e.message)}
  }

  window.onload=()=>{const s=localStorage.getItem('wa_token');if(s){T=s;document.getElementById('tok').value=s;init();}};
</script>
</body></html>`)
}

// GET/POST /config — Baca/tulis konfigurasi gateway
func handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		cfgMu.RLock()
		cfg := gwConfig
		cfgMu.RUnlock()
		jsonOK(w, cfg)
		return
	}
	// POST
	var newCfg GatewayConfig
	if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
		jsonError(w, "JSON tidak valid: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfgMu.Lock()
	gwConfig = newCfg
	cfgMu.Unlock()
	saveConfig()
	jsonOK(w, map[string]string{"message": "Konfigurasi tersimpan"})
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
		statsMu.Lock()
		totalFailed++
		statsMu.Unlock()
		jsonError(w, "Gagal kirim pesan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	statsMu.Lock()
	totalSent++
	lastSentAt = time.Now()
	lastSentTo = target
	lastSentType = "text"
	statsMu.Unlock()
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
		statsMu.Lock()
		totalFailed++
		statsMu.Unlock()
		jsonError(w, "Gagal kirim media: "+err.Error(), http.StatusInternalServerError)
		return
	}
	statsMu.Lock()
	totalSent++
	lastSentAt = time.Now()
	lastSentTo = target
	lastSentType = "image_url"
	statsMu.Unlock()
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
		statsMu.Lock()
		totalFailed++
		statsMu.Unlock()
		jsonError(w, "Gagal kirim gambar: "+err.Error(), http.StatusInternalServerError)
		return
	}
	statsMu.Lock()
	totalSent++
	lastSentAt = time.Now()
	lastSentTo = target
	lastSentType = "image_base64"
	statsMu.Unlock()
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
