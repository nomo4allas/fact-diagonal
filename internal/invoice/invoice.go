// Package invoice define el modelo de datos de una factura electrónica
// (DIAN/UBL) y utilidades para consolidar la información extraída desde las
// distintas fuentes (XML, texto nativo del PDF, OCR o Gemini).
package invoice

import (
	"strings"
	"unicode"
)

// Source identifica el origen del que se extrajo un conjunto de datos.
type Source string

const (
	SourceXML       Source = "xml"
	SourcePDFNative Source = "pdf-texto-nativo"
	SourceOCR       Source = "ocr-tesseract"
	SourceGemini    Source = "gemini"
)

// Data agrupa los campos de interés de una factura. Todos son cadenas para
// preservar la representación original (NITs con guiones, montos con
// separadores, etc.) sin imponer un formato numérico prematuro.
type Data struct {
	Numero       string // número de factura (cbc:ID completo, p.ej. "FE470")
	Prefijo      string // prefijo alfanumérico (p.ej. "FE")
	NIT          string // NIT del proveedor (emisor)
	RazonSocial  string // razón social del proveedor
	FechaEmision string // fecha de emisión (idealmente YYYY-MM-DD)
	ValorTotal   string // valor total a pagar
	CUFE         string // código único de factura electrónica
	Source       Source // origen de esta extracción
}

// camposClave son los campos que consideramos imprescindibles para dar una
// factura por "completa".
func (d Data) camposClave() map[string]string {
	return map[string]string{
		"numero":       d.Numero,
		"nit":          d.NIT,
		"razon_social": d.RazonSocial,
		"fecha":        d.FechaEmision,
		"valor_total":  d.ValorTotal,
		"cufe":         d.CUFE,
	}
}

// IsComplete indica si todos los campos clave están presentes.
func (d Data) IsComplete() bool {
	return len(d.MissingFields()) == 0
}

// MissingFields devuelve los nombres de los campos clave que faltan.
func (d Data) MissingFields() []string {
	var missing []string
	for name, v := range d.camposClave() {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// FilledCount cuenta cuántos campos clave tienen valor; sirve para comparar
// la "riqueza" de dos extracciones de la misma factura.
func (d Data) FilledCount() int {
	n := 0
	for _, v := range d.camposClave() {
		if strings.TrimSpace(v) != "" {
			n++
		}
	}
	return n
}

// Merge devuelve una copia de d completando cada campo vacío con el valor
// correspondiente de other. d tiene prioridad: sus valores no se sobrescriben.
// El Source resultante es el de d (la fuente preferente).
func (d Data) Merge(other Data) Data {
	out := d
	if strings.TrimSpace(out.Numero) == "" {
		out.Numero = other.Numero
	}
	if strings.TrimSpace(out.Prefijo) == "" {
		out.Prefijo = other.Prefijo
	}
	if strings.TrimSpace(out.NIT) == "" {
		out.NIT = other.NIT
	}
	if strings.TrimSpace(out.RazonSocial) == "" {
		out.RazonSocial = other.RazonSocial
	}
	if strings.TrimSpace(out.FechaEmision) == "" {
		out.FechaEmision = other.FechaEmision
	}
	if strings.TrimSpace(out.ValorTotal) == "" {
		out.ValorTotal = other.ValorTotal
	}
	if strings.TrimSpace(out.CUFE) == "" {
		out.CUFE = other.CUFE
	}
	return out
}

// DerivePrefijo, si Prefijo está vacío y Numero tiene la forma "letras+dígitos"
// (p.ej. "FE470"), separa el prefijo alfabético inicial.
func (d *Data) DerivePrefijo() {
	if strings.TrimSpace(d.Prefijo) != "" || d.Numero == "" {
		return
	}
	num := strings.TrimSpace(d.Numero)
	cut := 0
	for i, r := range num {
		if unicode.IsLetter(r) {
			cut = i + 1
			continue
		}
		break
	}
	if cut > 0 && cut < len(num) {
		d.Prefijo = num[:cut]
	}
}
