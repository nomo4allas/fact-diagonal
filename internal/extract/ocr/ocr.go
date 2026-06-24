// Package ocr es el segundo eslabón de la cascada: aplica OCR a PDFs sin capa
// de texto (escaneados). Usa binarios externos para no arrastrar CGO:
//
//   - pdftoppm (poppler) para rasterizar el PDF a imágenes PNG.
//   - tesseract para reconocer el texto de cada imagen.
//
// Si alguno de los binarios no está disponible, Available() devuelve false y la
// cascada continúa con el siguiente eslabón (Gemini). De este modo el Módulo 2
// funciona aunque el OCR no esté instalado en el entorno.
package ocr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nomo4allas/fact-diagonal/internal/extract/textparse"
	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

// Engine encapsula la configuración del OCR.
type Engine struct {
	Lang string // idiomas de Tesseract, p.ej. "spa+eng"
	DPI  int    // resolución de rasterizado (300 es un buen compromiso)
}

// New crea un Engine con valores por defecto razonables para facturas en
// español.
func New() *Engine {
	return &Engine{Lang: "spa+eng", DPI: 300}
}

// Available indica si los binarios necesarios (pdftoppm y tesseract) están en
// el PATH. La cascada lo consulta antes de intentar el OCR.
func (e *Engine) Available() bool {
	return hasBin("pdftoppm") && hasBin("tesseract")
}

// MissingTools devuelve los binarios requeridos que no se encontraron, para
// poder informarlo en el log.
func (e *Engine) MissingTools() []string {
	var missing []string
	for _, b := range []string{"pdftoppm", "tesseract"} {
		if !hasBin(b) {
			missing = append(missing, b)
		}
	}
	return missing
}

// Extract aplica OCR al PDF y parsea los campos de la factura.
func (e *Engine) Extract(ctx context.Context, pdf []byte) (invoice.Data, string, error) {
	text, err := e.ExtractText(ctx, pdf)
	if err != nil {
		return invoice.Data{}, "", err
	}
	d := textparse.Parse(text)
	d.Source = invoice.SourceOCR
	return d, text, nil
}

// ExtractText rasteriza el PDF y ejecuta Tesseract sobre cada página,
// devolviendo el texto reconocido concatenado.
func (e *Engine) ExtractText(ctx context.Context, pdf []byte) (string, error) {
	if !e.Available() {
		return "", fmt.Errorf("OCR no disponible, faltan binarios: %s", strings.Join(e.MissingTools(), ", "))
	}

	dir, err := os.MkdirTemp("", "fact-ocr-*")
	if err != nil {
		return "", fmt.Errorf("no se pudo crear directorio temporal: %w", err)
	}
	defer os.RemoveAll(dir)

	pdfPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(pdfPath, pdf, 0o600); err != nil {
		return "", fmt.Errorf("no se pudo escribir el PDF temporal: %w", err)
	}

	// 1) Rasterizar: produce page-1.png, page-2.png, …
	prefix := filepath.Join(dir, "page")
	cmd := exec.CommandContext(ctx, "pdftoppm", "-r", itoa(e.DPI), "-png", pdfPath, prefix)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("pdftoppm falló: %w: %s", err, strings.TrimSpace(string(out)))
	}

	pages, err := filepath.Glob(prefix + "*.png")
	if err != nil || len(pages) == 0 {
		return "", fmt.Errorf("el rasterizado no produjo imágenes")
	}
	sort.Strings(pages)

	// 2) OCR por página.
	var sb strings.Builder
	for _, img := range pages {
		cmd := exec.CommandContext(ctx, "tesseract", img, "stdout", "-l", e.Lang)
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("tesseract falló en %s: %w", filepath.Base(img), err)
		}
		sb.Write(out)
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}

func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
