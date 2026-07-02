// Package textparse extrae heurísticamente los campos de una factura a partir
// de texto plano (el obtenido del PDF nativo o del OCR). A diferencia del XML,
// el texto carece de estructura garantizada, por lo que estos resultados son
// de menor confianza y se usan como respaldo o validación del XML.
package textparse

import (
	"regexp"
	"strings"

	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

var (
	// CUFE: cadena hexadecimal larga (la DIAN usa SHA-384 → 96 hex chars).
	reCUFE = regexp.MustCompile(`(?i)cufe\D{0,12}([0-9a-f]{80,120})`)
	// Fallback CUFE: cualquier corrida hex de 90+ caracteres.
	reHexLong = regexp.MustCompile(`\b[0-9a-f]{90,120}\b`)

	// NIT del proveedor (admite puntos y guion de verificación).
	reNIT = regexp.MustCompile(`(?i)nit\D{0,8}(\d[\d\.\-]{6,15}\d)`)

	// Número de factura: "Factura ... FE470" / "No. FE-470".
	reNumero = regexp.MustCompile(`(?i)(?:factura(?:\s+(?:electr[oó]nica|de\s+venta))?|no\.?|n[uú]mero)\D{0,6}?([A-Z]{1,6}[-\s]?\d{1,12})`)

	// Fecha de emisión en formato ISO (YYYY-MM-DD).
	reFechaISO = regexp.MustCompile(`(?i)(?:fecha\s+(?:de\s+)?(?:emisi[oó]n|generaci[oó]n)\D{0,6})?(\d{4}-\d{2}-\d{2})`)

	// Valor total: "Total $ 1.234.567,00".
	reTotal = regexp.MustCompile(`(?i)(?:valor\s+)?(?:total\s+(?:a\s+pagar|factura|neto)?|total)\D{0,6}\$?\s*([\d][\d\.,]{2,})`)

	// Ajuste Módulo 2 — campos adicionales del PDF.
	// PEDIDO: "PEDIDO No: 12345" (admite "No", "No.", "N°", "Nro" o nada).
	rePedido = regexp.MustCompile(`(?i)pedido\s*(?:n[o°º\.]*|nro\.?|n[uú]mero)?\s*:?\s*([A-Z0-9][A-Z0-9\-/]{1,29})`)
	// DECLARAC: "DECLARAC: ABC-123".
	reDeclarac = regexp.MustCompile(`(?i)declarac(?:i[oó]n)?\s*:?\s*([A-Z0-9][A-Z0-9\-/]{1,29})`)
	// BL / Bill of Lading: aparece como "DOCTTE:" o "N° BL:" según el proveedor.
	reBL = regexp.MustCompile(`(?i)(?:doctte|(?:n[o°º\.]*\s*)?b\.?\s*l\.?|bill\s+of\s+lading)\s*:?\s*([A-Z0-9][A-Z0-9\-/]{2,29})`)
)

// Parse devuelve los campos detectables en el texto. El Source debe fijarlo el
// llamador (pdf-texto-nativo u ocr) según su origen.
func Parse(text string) invoice.Data {
	var d invoice.Data

	if m := reCUFE.FindStringSubmatch(text); m != nil {
		d.CUFE = strings.ToLower(m[1])
	} else if m := reHexLong.FindString(text); m != "" {
		d.CUFE = strings.ToLower(m)
	}

	if m := reNIT.FindStringSubmatch(text); m != nil {
		d.NIT = cleanNIT(m[1])
	}

	if m := reNumero.FindStringSubmatch(text); m != nil {
		d.Numero = normalizeNumero(m[1])
	}

	if m := reFechaISO.FindStringSubmatch(text); m != nil {
		d.FechaEmision = m[1]
	}

	if m := reTotal.FindStringSubmatch(text); m != nil {
		d.ValorTotal = strings.TrimSpace(m[1])
	}

	// Ajuste Módulo 2 — campos adicionales del PDF.
	if m := rePedido.FindStringSubmatch(text); m != nil {
		d.Pedido = strings.TrimSpace(m[1])
	}
	if m := reDeclarac.FindStringSubmatch(text); m != nil {
		d.Declarac = strings.TrimSpace(m[1])
	}
	if m := reBL.FindStringSubmatch(text); m != nil {
		d.BL = strings.TrimSpace(m[1])
	}

	d.DerivePrefijo()
	return d
}

func cleanNIT(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), ".-")
}

// normalizeNumero une el prefijo y el consecutivo eliminando separadores
// intermedios ("FE-470" → "FE470", "FE 470" → "FE470").
func normalizeNumero(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	return strings.ToUpper(s)
}
