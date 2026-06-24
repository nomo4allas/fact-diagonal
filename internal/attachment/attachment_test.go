package attachment

import (
	"archive/zip"
	"bytes"
	"testing"
)

// makeZIP arma en memoria un ZIP con los archivos indicados (nombre→contenido).
func makeZIP(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestFromZIP_PDFyXML(t *testing.T) {
	data := makeZIP(t, map[string]string{
		"factura.pdf": "%PDF-1.4 contenido",
		"factura.xml": "<Invoice/>",
	})
	bundles, err := FromZIP("adj.zip", data)
	if err != nil {
		t.Fatalf("FromZIP error: %v", err)
	}
	if len(bundles) != 1 {
		t.Fatalf("se esperaba 1 bundle, hay %d", len(bundles))
	}
	b := bundles[0]
	if !b.HasPDF() || !b.HasXML() {
		t.Errorf("el bundle debería traer PDF y XML: %+v", b)
	}
}

func TestFromZIP_NombresDistintos(t *testing.T) {
	// Caso DIAN: PDF y XML con nombres base distintos pero un único par.
	data := makeZIP(t, map[string]string{
		"ar_900123456_FE470.pdf": "%PDF-1.4",
		"fv_900123456_FE470.xml": "<Invoice/>",
	})
	bundles, err := FromZIP("adj.zip", data)
	if err != nil {
		t.Fatalf("FromZIP error: %v", err)
	}
	if len(bundles) != 1 || !bundles[0].HasPDF() || !bundles[0].HasXML() {
		t.Errorf("se esperaba 1 bundle con PDF+XML, hay %d: %+v", len(bundles), bundles)
	}
}

func TestClasificacion(t *testing.T) {
	if !IsZIP("x.ZIP", "") {
		t.Error("x.ZIP debería ser ZIP")
	}
	if !IsPDF("x.pdf", "application/octet-stream") {
		t.Error("x.pdf debería ser PDF")
	}
	if !IsPDF("sinext", "application/pdf") {
		t.Error("contentType application/pdf debería ser PDF")
	}
	if IsRelevant("nota.txt", "text/plain") {
		t.Error("un .txt no debería ser relevante")
	}
}
