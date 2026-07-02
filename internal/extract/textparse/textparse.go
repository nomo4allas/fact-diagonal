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
	// Los tres regex se ANCLAN a la MISMA línea: entre la etiqueta y el valor solo
	// se permiten espacios/tabuladores ([ \t]), nunca saltos de línea, para no
	// capturar el contenido de la línea siguiente (p.ej. "PEDIDO No:\nFECHA…").
	// El valor capturado se valida además contra palabrasReservadas (ver Parse).
	//
	// PEDIDO: "PEDIDO No: 12345" (admite "No", "No.", "N°", "Nro" o nada).
	rePedido = regexp.MustCompile(`(?i)pedido[ \t]*(?:n[o°º]?\.?|nro\.?|n[uú]mero)?[ \t]*:?[ \t]*([A-Z0-9][A-Z0-9\-/]{1,29})`)
	// DECLARAC: "DECLARAC: ABC-123".
	reDeclarac = regexp.MustCompile(`(?i)declarac(?:i[oó]n)?[ \t]*:?[ \t]*([A-Z0-9][A-Z0-9\-/]{1,29})`)
	// BL / Bill of Lading: "DOCTTE:" o "N° BL:". El token "BL" se ancla con \b para
	// no dispararse dentro de palabras (p.ej. la secuencia "bl" de "oBLigaciones").
	reBL = regexp.MustCompile(`(?i)(?:doctte|bill[ \t]+of[ \t]+lading|(?:n[o°º]?\.?[ \t]*)?\bbl\b)[ \t]*:?[ \t]*([A-Z0-9][A-Z0-9\-/]{2,29})`)
)

// palabrasReservadas son términos que NO son un Pedido/DECLARAC/BL real: suelen
// colarse por la proximidad de otra celda o etiqueta del documento (p.ej. capturar
// "FECHA" como pedido, o "DECLARAC" como BL). Cualquier valor que coincida con
// una de ellas se descarta. La comparación es en mayúsculas.
var palabrasReservadas = map[string]bool{
	"FECHA": true, "DECLARAC": true, "DECLARACION": true, "DECLARACIÓN": true,
	"NIT": true, "PEDIDO": true, "CUFE": true, "TOTAL": true, "DOCTTE": true,
	"BL": true, "FACTURA": true, "VENCIMIENTO": true,
	// Fragmentos de las propias etiquetas ("PEDIDO No:", "Nro") que el regex puede
	// dejar como valor cuando el token es opcional y no hay valor real en la línea.
	"NO": true, "N": true, "NRO": true, "NUMERO": true, "NÚMERO": true,
}

// valorExtraValido descarta valores vacíos o que sean una palabra reservada.
func valorExtraValido(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	return !palabrasReservadas[strings.ToUpper(v)]
}

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

	// Ajuste Módulo 2 — campos adicionales del PDF. Cada valor capturado se valida
	// contra palabrasReservadas para evitar falsos positivos (FECHA, DECLARAC, …).
	if m := rePedido.FindStringSubmatch(text); m != nil && valorExtraValido(m[1]) {
		d.Pedido = strings.TrimSpace(m[1])
	}
	if m := reDeclarac.FindStringSubmatch(text); m != nil && valorExtraValido(m[1]) {
		d.Declarac = strings.TrimSpace(m[1])
	}
	if m := reBL.FindStringSubmatch(text); m != nil && valorExtraValido(m[1]) {
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
