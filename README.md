# 📶 GDI Ledger - Premium WiFi Iuran

Sistem pencatatan dan manajemen iuran WiFi bersama berbasis web dengan tema modern, klasik, dan profesional.

![Version](https://img.shields.io/badge/version-2.0-amber)
![License](https://img.shields.io/badge/license-MIT-blue)
![Firebase](https://img.shields.io/badge/backend-Firebase-orange)

## 🌟 Fitur Utama

### 💳 Manajemen Pembayaran
- **Tracking Bulanan**: Catat pembayaran iuran per bulan dengan status lengkap
- **Multi-Status**: Belum Tercatat, Belum Lunas, dan Lunas
- **Bukti Pembayaran**: Upload foto bukti transfer dengan kompresi otomatis
- **Kwitansi Digital**: Generate kwitansi otomatis dengan logo dan tanda tangan digital

### 👥 Profil Pengguna
- **Pemilik WiFi (Ferdi)**: Profil dengan aksen amber/gold
- **Mitra (Jho/Joventaluk)**: Profil dengan aksen sky blue
- **Informasi Layanan IndiHome**: Kecepatan 150 Mbps, Tagihan Rp 424.800/bulan

### 🔐 Sistem Keamanan
- **Mode Admin**: Login dengan Firebase Authentication
- **Mode Tamu**: Akses read-only untuk melihat status pembayaran
- **Firebase Security Rules**: Kontrol akses data di level database

### 📱 Notifikasi WhatsApp
- **Reminder Otomatis**: Kirim pengingat pembayaran via WhatsApp (Fonnte API)
- **Konfirmasi Lunas**: Notifikasi otomatis dengan lampiran kwitansi saat pembayaran dikonfirmasi
- **Kwitansi Media**: Upload otomatis kwitansi ke Firebase Storage dan kirim via WhatsApp

### 🎨 Design Premium
- **Dark Theme**: Gradasi gelap yang nyaman di mata
- **Glass-morphism**: Efek blur dan transparency modern
- **Gold Accent**: Warna amber/gold untuk kesan premium
- **Responsive**: Optimasi untuk desktop dan mobile
- **Font Modern**: Inter (body) + Playfair Display (heading)

## 🛠️ Teknologi

- **Frontend**: HTML5, TailwindCSS, Lucide Icons
- **Backend**: Firebase (Firestore, Authentication, Storage)
- **Notifications**: Fonnte WhatsApp API
- **Image Export**: html2canvas
- **Hosting**: Vercel

## 📋 Prasyarat

- Akun Firebase dengan Firestore, Authentication, dan Storage aktif
- Token Fonnte untuk WhatsApp API (opsional)
- Domain/hosting (contoh: Vercel)

## 🚀 Instalasi

### 1. Clone Repository
```bash
git clone https://github.com/RoseRina/Iuran-Wifi.git
cd Iuran-Wifi
```

### 2. Konfigurasi Firebase

Buka `index.html` dan update Firebase config:

```javascript
const firebaseConfig = {
    apiKey: "YOUR_API_KEY",
    authDomain: "YOUR_PROJECT.firebaseapp.com",
    projectId: "YOUR_PROJECT_ID",
    storageBucket: "YOUR_PROJECT.firebasestorage.app",
    messagingSenderId: "YOUR_SENDER_ID",
    appId: "YOUR_APP_ID"
};
```

### 3. Konfigurasi Firestore Security Rules

Di Firebase Console > Firestore Database > Rules:

```javascript
rules_version = '2';
service cloud.firestore {
  match /databases/{database}/documents {
    match /artifacts/{appId}/public/data/payments/{document=**} {
      // Mode tamu bisa read, admin bisa write
      allow read: if request.auth != null;
      allow write: if request.auth != null && request.auth.token.email_verified == true;
    }
  }
}
```

### 4. Konfigurasi Storage Rules

Di Firebase Console > Storage > Rules:

```javascript
rules_version = '2';
service firebase.storage {
  match /b/{bucket}/o {
    match /receipts/{receiptId} {
      allow read: if request.auth != null;
      allow write: if request.auth != null && request.auth.token.email_verified == true;
    }
  }
}
```

### 5. Konfigurasi WhatsApp (Opsional)

Update token Fonnte di `index.html`:

```javascript
const fonnteToken = "YOUR_FONNTE_TOKEN";
const jhoWA = "6282149335323"; // Nomor WhatsApp tujuan
```

### 6. Setup Admin Account

Di Firebase Console > Authentication:
1. Klik "Add User"
2. Masukkan email dan password admin
3. Verify email jika diperlukan

### 7. Upload Aset

Upload file berikut ke root folder:
- `FERDI.jpeg` - Foto profil pemilik WiFi
- `Indihome.png` - Logo IndiHome (transparan)

### 8. Deploy

#### Vercel (Recommended)
```bash
npm install -g vercel
vercel
```

#### Manual
Upload semua file ke hosting pilihan Anda.

## 📖 Cara Penggunaan

### Mode Tamu
1. Buka website
2. Lihat status pembayaran per bulan
3. Klik kotak bulan untuk melihat kwitansi (jika sudah lunas)

### Mode Admin
1. Klik tombol **"Masuk"**
2. Login dengan email/password admin
3. Kelola pembayaran:
   - **Klik kotak bulan** untuk input/edit data
   - **Upload foto bukti** pembayaran
   - **Ubah status** menjadi Lunas/Belum Lunas
   - **Kirim reminder** WhatsApp untuk tagihan belum lunas (ikon bell)

### Fitur Admin Tambahan
- **Auto-send WhatsApp**: Saat pembayaran dikonfirmasi lunas, sistem otomatis kirim notifikasi + kwitansi ke WhatsApp
- **Download Kwitansi**: Export kwitansi sebagai gambar PNG
- **Hapus Record**: Hapus data pembayaran (tombol muncul di modal edit)

## ⚙️ Konfigurasi Lanjutan

### Ubah Nominal Iuran
Di `index.html`, cari dan ubah:
```javascript
document.getElementById('edit-amount').value = 150000; // Ubah sesuai kebutuhan
```

### Ubah Payer
Di `index.html`, cari dan ubah:
```javascript
const payer = "Jho/Joventaluk"; // Ubah nama
```

### Ubah Metadata IndiHome
Edit bagian HTML di section "Informasi Layanan WiFi":
```html
<p class="text-sm font-black text-white">FERDI</p> <!-- Nama Pelanggan -->
<p class="text-sm font-black text-emerald-400">150 Mbps</p> <!-- Kecepatan -->
<p class="text-lg font-black gold-text">Rp 424.800</p> <!-- Tagihan -->
```

## 🔒 Catatan Keamanan

> ⚠️ **PERINGATAN**: Konfigurasi saat ini mengekspos token API di client-side. Untuk production lebih aman:

1. **Firebase API Key**: 
   - Batasi dengan domain restrictions di Google Console
   - Gunakan Firebase Security Rules yang ketat
   
2. **Fonnte Token**:
   - Pindahkan ke backend (Cloud Functions)
   - Atau gunakan Vercel Serverless Functions
   
3. **Authentication**:
   - Aktifkan email verification
   - Gunakan reCAPTCHA untuk anti-bot

## 📱 Screenshot

*Tampilan disesuaikan dengan tema gelap modern dengan aksen gold premium*

## 🤝 Kontribusi

Pull requests dipersilakan. Untuk perubahan besar, harap buka issue terlebih dahulu.

## 📄 Lisensi

MIT License - lihat file LICENSE untuk detail

## 👨‍💻 Developer

**Ferdi**  
📧 Contact: [via GitHub](https://github.com/RoseRina)

## 🔄 Changelog

### Version 2.0 (2026-01-29)
- ✨ Redesign total dengan tema gelap premium
- 🎨 Font upgrade: Inter + Playfair Display
- 🏠 Tambah profil Ferdi (pemilik WiFi)
- 📡 Tambah informasi layanan IndiHome
- ⚡ Optimasi loading dengan fallback mechanism
- 🔔 Auto-send kwitansi via WhatsApp
- 🖼️ Kompresi image otomatis
- 📱 Responsive design improvements

### Version 1.0
- 🚀 Rilis awal dengan fitur dasar
- 💳 Tracking pembayaran bulanan
- 🔐 Admin authentication
- 📄 Generate kwitansi

---

**⭐ Jika project ini membantu, berikan star di GitHub!**
