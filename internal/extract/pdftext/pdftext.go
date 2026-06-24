// Package pdftext obtiene la capa de texto nativa de un PDF y, a partir de
// ella, los campos de la factura. Es el primer eslabón de la cascada de
// extracción: rápido y sin dependencias externas, pero solo funciona cuando el
// PDF trae texto seleccionable (no escaneado/imagen).
package pdftext

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/ledongthuc/pdf"

	"github.com/nomo4allas/fact-diagonal/internal/extract/textparse"
	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

// ExtractText devuelve el texto nativo del PDF. Devuelve error si el PDF no
// tiene capa de texto utilizable.
func ExtractText(data []byte) (text string, err error) {
	// La librería puede entrar en pánico ante PDFs malformados; lo contenemos
	// para no derribar el proceso y tratarlo como "sin texto".
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("pánico al leer el PDF: %v", r)
		}
	}()

	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("no se pudo abrir el PDF: %w", err)
	}

	reader, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("no se pudo extraer texto del PDF: %w", err)
	}
	var sb strings.Builder
	if _, err := io.Copy(&sb, reader); err != nil {
		return "", fmt.Errorf("error leyendo el texto del PDF: %w", err)
	}
	return sb.String(), nil
}

// HasUsableText indica si el texto extraído tiene contenido suficiente como
// para intentar el parseo (umbral conservador para distinguir un PDF de texto
// de uno escaneado que devuelve casi nada).
func HasUsableText(text string) bool {
	return len(strings.TrimSpace(text)) >= 40
}

// Extract obtiene el texto nativo y parsea los campos de la factura.
func Extract(data []byte) (invoice.Data, string, error) {
	text, err := ExtractText(data)
	if err != nil {
		return invoice.Data{}, "", err
	}
	if !HasUsableText(text) {
		return invoice.Data{}, text, fmt.Errorf("el PDF no tiene texto nativo utilizable (posible escaneo)")
	}
	d := textparse.Parse(text)
	d.Source = invoice.SourcePDFNative
	return d, text, nil
}
