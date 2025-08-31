package main

import (
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/csv"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/sessions"
	"github.com/jung-kurt/gofpdf"
	qrcode "github.com/skip2/go-qrcode"
	_ "modernc.org/sqlite"
)

var (
	db         *sql.DB
	tplIndex   *template.Template
	tplPayroll *template.Template
	tplLogin   *template.Template
)

// directories
var qrDir = "data/qrs"
var pdfDir = "data/pdf"

//go:embed tmpl/*
var tplFS embed.FS

// ---------- AUTH ----------
var adminUser = "slacadmin"
var adminPass = "slacadmin1234"

// NOTE: Replace the secret with a strong random key in production
var store = sessions.NewCookieStore([]byte("super-secret-key-please-change"))

// ---------- MAIN ----------
func main() {
	// timezone
	loc, _ := time.LoadLocation("Asia/Manila")
	time.Local = loc

	// create dirs
	if err := os.MkdirAll(qrDir, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(pdfDir, 0o755); err != nil {
		log.Fatal(err)
	}

	// open DB
	var err error
	db, err = sql.Open("sqlite", "file:data/dtr.db?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	if err := initSchema(); err != nil {
		log.Fatal(err)
	}

	// parse templates from embed (tmpl/)
	tplIndex = mustTemplate("tmpl/index.html")
	tplPayroll = mustTemplate("tmpl/payroll.html")
	// login template should exist at tmpl/login.html (use the login template you added)
	tplLogin = mustTemplate("tmpl/login.html")

	// session options
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   60 * 60, // 1 hour
		HttpOnly: true,
	}

	// routes
	http.HandleFunc("/login", handleLoginPage)
	http.HandleFunc("/logout", handleLogout)

	// Admin-protected routes
	http.HandleFunc("/", requireLogin(handleHome))
	http.HandleFunc("/faculty/add", requireLogin(handleFacultyAdd))
	http.HandleFunc("/faculty/toggle", requireLogin(handleFacultyToggle))
	http.HandleFunc("/faculty/delete", requireLogin(handleFacultyDelete))
	http.HandleFunc("/print-qrs.pdf", requireLogin(handlePrintQRCards))
	http.HandleFunc("/payroll", requireLogin(handlePayroll))
	http.HandleFunc("/payroll.csv", requireLogin(handlePayrollCSV))

	// Public/scan resources
	http.HandleFunc("/scan/", handleScan)
	http.HandleFunc("/qrs/", handleQRFile)
	// serve logo / images from img/ directory
	http.Handle("/img/", http.StripPrefix("/img/", http.FileServer(http.Dir("img/"))))

	log.Println("✅ Server running at http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func mustTemplate(name string) *template.Template {
	b, err := tplFS.ReadFile(name)
	if err != nil {
		log.Fatal(err)
	}
	tpl, err := template.New(filepath.Base(name)).Parse(string(b))
	if err != nil {
		log.Fatal(err)
	}
	return tpl
}

// ---------- DB INIT ----------
func initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS faculty (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT,
		role TEXT,
		rate_per_hour REAL,
		active INTEGER DEFAULT 1,
		token TEXT UNIQUE
	);
	CREATE TABLE IF NOT EXISTS dtr (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		faculty_id INTEGER,
		in_time DATETIME,
		out_time DATETIME
	);
	`
	_, err := db.Exec(schema)
	return err
}

// ---------- AUTH MIDDLEWARE ----------
func requireLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "session")
		if auth, ok := session.Values["authenticated"].(bool); !ok || !auth {
			// if request is already to /login, let it pass
			if r.URL.Path == "/login" {
				next.ServeHTTP(w, r)
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// ---------- HANDLERS ----------
// GET/POST login page (uses embedded tmpl/login.html)
func handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		_ = tplLogin.Execute(w, nil)
		return
	}
	if r.Method == http.MethodPost {
		username := r.FormValue("username")
		password := r.FormValue("password")

		if username == adminUser && password == adminPass {
			session, _ := store.Get(r, "session")
			session.Values["authenticated"] = true
			_ = session.Save(r, w)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		// show login with error
		_ = tplLogin.Execute(w, map[string]string{"Error": "Invalid username or password"})
		return
	}
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// Logout
func handleLogout(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session")
	session.Values["authenticated"] = false
	_ = session.Save(r, w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func handleHome(w http.ResponseWriter, r *http.Request) {
	rows, _ := db.Query("SELECT id,name,role,rate_per_hour,active,token FROM faculty")
	defer rows.Close()

	type Faculty struct {
		ID          int
		Name        string
		Role        string
		RatePerHour float64
		Active      bool
		Token       string
	}
	var faculty []Faculty
	for rows.Next() {
		var f Faculty
		rows.Scan(&f.ID, &f.Name, &f.Role, &f.RatePerHour, &f.Active, &f.Token)
		faculty = append(faculty, f)
	}

	data := struct {
		Today   string
		Faculty []Faculty
	}{
		Today:   time.Now().Format("2006-01-02"),
		Faculty: faculty,
	}

	tplIndex.Execute(w, data)
}

func handleFacultyAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 400)
		return
	}
	name := r.FormValue("name")
	role := r.FormValue("role")
	rateStr := r.FormValue("rate")
	rate, _ := strconv.ParseFloat(rateStr, 64)

	token := randToken()
	_, err := db.Exec("INSERT INTO faculty (name,role,rate_per_hour,token) VALUES (?,?,?,?)",
		name, role, rate, token)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// generate QR with scan URL + readable info
	payload := fmt.Sprintf("http://%s/scan/%s\nName: %s\nRole: %s", r.Host, token, name, role)
	qrFile := filepath.Join(qrDir, token+".png")
	_ = qrcode.WriteFile(payload, qrcode.Medium, 256, qrFile)

	http.Redirect(w, r, "/", 302)
}

func handleFacultyToggle(w http.ResponseWriter, r *http.Request) {
	// Accept id from POST form value instead of URL query
	id := r.FormValue("id")
	if id == "" {
		http.Error(w, "Missing id", http.StatusBadRequest)
		return
	}

	// Flip active flag
	_, err := db.Exec("UPDATE faculty SET active=1-active WHERE id=?", id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Fetch updated faculty status
	var active int
	err = db.QueryRow("SELECT active FROM faculty WHERE id=?", id).Scan(&active)
	if err != nil {
		http.Error(w, "Faculty not found", http.StatusNotFound)
		return
	}

	// Return JSON result
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":%s,"active":%t}`, id, active == 1)
}

func handleFacultyDelete(w http.ResponseWriter, r *http.Request) {
	// Accept id from POST form value
	id := r.FormValue("id")
	if id == "" {
		http.Error(w, "Missing id", http.StatusBadRequest)
		return
	}

	// Delete faculty by id
	_, err := db.Exec("DELETE FROM faculty WHERE id=?", id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Redirect back to home page after deletion
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleScan(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/scan/")

	var fid int
	var name, role string
	err := db.QueryRow("SELECT id,name,role FROM faculty WHERE token=?", token).Scan(&fid, &name, &role)
	if err != nil {
		http.Error(w, "Faculty not found", 404)
		return
	}

	now := time.Now()
	var dtrID int
	var inTime sql.NullTime
	err = db.QueryRow("SELECT id,in_time FROM dtr WHERE faculty_id=? AND out_time IS NULL ORDER BY in_time DESC LIMIT 1", fid).Scan(&dtrID, &inTime)

	status := ""
	if err == sql.ErrNoRows {
		// clock IN
		_, _ = db.Exec("INSERT INTO dtr(faculty_id,in_time) VALUES (?,?)", fid, now)
		status = "Clock IN"
	} else if err == nil {
		// clock OUT
		_, _ = db.Exec("UPDATE dtr SET out_time=? WHERE id=?", now, dtrID)
		status = "Clock OUT"
	} else {
		http.Error(w, "DB error", 500)
		return
	}

	// show friendly HTML page
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `
		<!doctype html>
		<html><head><meta charset="utf-8"/>
		<title>Scan Result</title></head><body>
		<h2>%s</h2>
		<p><b>%s</b> (%s)</p>
		<p>%s at %s</p>
		<p><a href="/">← Back to Home</a></p>
		</body></html>
	`, status, name, role, status, now.Format("2006-01-02 15:04:05"))
}

func handleQRFile(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(qrDir, filepath.Base(r.URL.Path)))
}

func handlePrintQRCards(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query("SELECT id, name, role, token FROM faculty WHERE active=1")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	pdf := gofpdf.New("P", "mm", "A4", "")
	// NOTE: ensure fonts/Roboto-Regular.ttf exists or change font
	pdf.AddUTF8Font("Roboto", "", "fonts/Roboto-Regular.ttf")
	pdf.SetFont("Roboto", "", 8)
	pdf.AddPage()

	// Card settings for 3×3 grid
	cardW := 60.0
	cardH := 70.0 // taller to fit QR under text
	marginX := 10.0
	marginY := 10.0
	spacingX := 8.0
	spacingY := 8.0
	cardsPerRow := 3
	cardsPerCol := 3

	x := marginX
	y := marginY
	col := 0
	row := 0

	for rows.Next() {
		var id int
		var name, role, token string
		rows.Scan(&id, &name, &role, &token)

		// Draw card border
		pdf.SetDrawColor(0, 0, 0)
		pdf.Rect(x, y, cardW, cardH, "D")

		// Faculty info (centered horizontally)
		pdf.SetXY(x, y+8)
		pdf.CellFormat(cardW, 5, name, "", 0, "C", false, 0, "")
		pdf.SetXY(x, y+14)
		pdf.CellFormat(cardW, 5, fmt.Sprintf("(%s)", role), "", 0, "C", false, 0, "")

		// QR code (centered under role)
		qrPath := filepath.Join(qrDir, token+".png")
		qrSize := 28.0
		qrX := x + (cardW-qrSize)/2
		qrY := y + 25
		pdf.ImageOptions(qrPath, qrX, qrY, qrSize, qrSize, false, gofpdf.ImageOptions{ImageType: "PNG"}, 0, "")

		// Move grid
		col++
		if col >= cardsPerRow {
			col = 0
			row++
			x = marginX
			y += cardH + spacingY
			if row >= cardsPerCol {
				pdf.AddPage()
				x = marginX
				y = marginY
				row = 0
			}
		} else {
			x += cardW + spacingX
		}
	}

	// Output PDF
	w.Header().Set("Content-Type", "application/pdf")
	if err := pdf.Output(w); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func handlePayroll(w http.ResponseWriter, r *http.Request) {
	start, _ := time.Parse("2006-01-02", r.FormValue("start"))
	end, _ := time.Parse("2006-01-02", r.FormValue("end"))
	if end.IsZero() {
		end = time.Now()
	}

	type Row struct {
		FacultyID   int
		Name        string
		Role        string
		RatePerHour float64
		TotalHours  float64
		Pay         float64
	}
	var rows []Row
	var grand float64

	q := `
	SELECT f.id, f.name, f.role, f.rate_per_hour, d.in_time, d.out_time
	FROM faculty f
	LEFT JOIN dtr d 
	  ON d.faculty_id = f.id
	  AND d.in_time BETWEEN ? AND ?
	`
	rs, err := db.Query(q, start, end)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rs.Close()

	m := map[int]*Row{}

	for rs.Next() {
		var id int
		var name, role string
		var rate float64
		var inT, outT sql.NullTime
		if err := rs.Scan(&id, &name, &role, &rate, &inT, &outT); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if _, ok := m[id]; !ok {
			m[id] = &Row{FacultyID: id, Name: name, Role: role, RatePerHour: rate}
		}
		if inT.Valid && outT.Valid {
			dur := outT.Time.Sub(inT.Time).Hours()
			if dur > 0 {
				m[id].TotalHours += dur
			}
		}
	}

	for _, r := range m {
		r.TotalHours = math.Round(r.TotalHours*4) / 4
		r.Pay = r.TotalHours * r.RatePerHour
		grand += r.Pay
		rows = append(rows, *r)
	}

	data := struct {
		Start, End string
		Rows       []Row
		GrandTotal float64
	}{
		Start:      start.Format("2006-01-02"),
		End:        end.Format("2006-01-02"),
		Rows:       rows,
		GrandTotal: grand,
	}

	tplPayroll.Execute(w, data)
}

func handlePayrollCSV(w http.ResponseWriter, r *http.Request) {
	start, _ := time.Parse("2006-01-02", r.FormValue("start"))
	end, _ := time.Parse("2006-01-02", r.FormValue("end"))
	if end.IsZero() {
		end = time.Now()
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment;filename=payroll.csv")
	csvw := csv.NewWriter(w)
	defer csvw.Flush()

	csvw.Write([]string{"FacultyID", "Name", "Role", "Rate/hr", "TotalHours", "Pay"})

	q := `
	SELECT f.id,f.name,f.role,f.rate_per_hour,d.in_time,d.out_time
	FROM faculty f
	LEFT JOIN dtr d ON d.faculty_id=f.id
	WHERE d.in_time BETWEEN ? AND ?
	`
	rs, _ := db.Query(q, start, end)
	defer rs.Close()
	m := map[int]struct {
		name  string
		role  string
		rate  float64
		hours float64
	}{}

	for rs.Next() {
		var id int
		var name, role string
		var rate float64
		var inT, outT sql.NullTime
		rs.Scan(&id, &name, &role, &rate, &inT, &outT)
		tmp := m[id]
		tmp.name, tmp.role, tmp.rate = name, role, rate
		if inT.Valid && outT.Valid {
			tmp.hours += outT.Time.Sub(inT.Time).Hours()
		}
		m[id] = tmp
	}

	for id, r := range m {
		h := math.Round(r.hours*4) / 4
		pay := h * r.rate
		csvw.Write([]string{
			strconv.Itoa(id), r.name, r.role,
			fmt.Sprintf("%.2f", r.rate),
			fmt.Sprintf("%.2f", h),
			fmt.Sprintf("%.2f", pay),
		})
	}
}

// ---------- UTIL ----------
func randToken() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
