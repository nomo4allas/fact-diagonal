// Package attachment clasifica los adjuntos de un correo y normaliza su
// contenido en "bundles" listos para extraer datos: cada bundle expone, como
// máximo, un PDF y un XML provenientes de la misma factura, más los demás
// archivos del ZIP (imágenes, DOCX, etc.) como "extras" para adjuntarlos.
//
// Soporta dos formas de entrega habituales en facturación electrónica DIAN:
//   - Adjunto ZIP que contiene el PDF de representación gráfica y el XML UBL
//     (y opcionalmente otros archivos: JPG/TIF/DOCX…). El ZIP puede anidar
//     otros ZIP; se descomprimen de forma recursiva hasta maxZIPDepth niveles.
//   - Adjunto PDF directo (sin XML de respaldo).
package attachment

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Bundle agrupa los documentos de una factura extraídos de un adjunto.
type Bundle struct {
	Origin  string // nombre del adjunto de origen (p.ej. "factura.zip")
	PDF     []byte // contenido del PDF, o nil si no hay
	PDFName string // nombre del PDF dentro del adjunto
	XML     []byte // contenido del XML, o nil si no hay
	XMLName string // nombre del XML dentro del adjunto
	// Extras son los demás archivos del ZIP que no son PDF ni XML (JPG, TIF,
	// DOCX, etc.). Se adjuntan a la misma factura para persistirlos en el
	// Módulo 3. Solo el primer bundle de un ZIP los recibe, para no duplicarlos.
	Extras []Extra
}

// Extra es un archivo del ZIP distinto de PDF/XML (imagen, DOCX, etc.) que
// también debe adjuntarse a la factura en el Módulo 3.
type Extra struct {
	Name string // nombre exacto del archivo dentro del ZIP
	Ext  string // extensión sin punto, en minúsculas (p.ej. "jpg", "docx")
	Data []byte // contenido binario del archivo
}

// HasPDF indica si el bundle trae un PDF.
func (b Bundle) HasPDF() bool { return len(b.PDF) > 0 }

// HasXML indica si el bundle trae un XML.
func (b Bundle) HasXML() bool { return len(b.XML) > 0 }

// IsZIP reporta si el nombre/tipo MIME corresponden a un ZIP.
func IsZIP(name, contentType string) bool {
	if ext := strings.ToLower(filepath.Ext(name)); ext == ".zip" {
		return true
	}
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "zip")
}

// IsPDF reporta si el nombre/tipo MIME corresponden a un PDF.
func IsPDF(name, contentType string) bool {
	if ext := strings.ToLower(filepath.Ext(name)); ext == ".pdf" {
		return true
	}
	return strings.Contains(strings.ToLower(contentType), "pdf")
}

// IsRelevant indica si el adjunto es procesable por el Módulo 2 (ZIP o PDF).
func IsRelevant(name, contentType string) bool {
	return IsZIP(name, contentType) || IsPDF(name, contentType)
}

// maxZIPDepth limita cuántos niveles de ZIP anidados se descomprimen. Es un
// tope de seguridad para evitar bucles o "ZIP bombs": el ZIP de entrada es el
// nivel 1 y cada ZIP interior suma uno.
const maxZIPDepth = 5

// doc es un PDF o XML ya extraído, identificado por su nombre dentro del ZIP.
type doc struct {
	name string
	data []byte
}

// FromZIP descomprime el ZIP y devuelve un bundle por cada PDF encontrado,
// emparejándolo con el XML del mismo nombre base si existe; un XML suelto sin
// PDF también genera su propio bundle. Esto cubre el caso típico (un PDF + un
// XML) y los menos comunes (varios documentos en un mismo ZIP). Los demás
// archivos del ZIP (imágenes, DOCX, etc.) se acumulan como Extras del primer
// bundle para adjuntarlos a la misma factura. Los ZIP anidados se recorren de
// forma recursiva hasta maxZIPDepth niveles.
func FromZIP(origin string, data []byte) ([]Bundle, error) {
	var pdfs, xmls, others []doc
	if err := collectDocs(origin, data, 1, &pdfs, &xmls, &others); err != nil {
		return nil, err
	}

	// Indexamos los XML por nombre base para emparejarlos con su PDF.
	xmlByBase := make(map[string]doc, len(xmls))
	for _, x := range xmls {
		xmlByBase[baseNoExt(x.name)] = x
	}

	var bundles []Bundle
	usedXML := make(map[string]bool)

	for _, p := range pdfs {
		b := Bundle{Origin: origin, PDF: p.data, PDFName: p.name}
		if x, ok := xmlByBase[baseNoExt(p.name)]; ok {
			b.XML, b.XMLName = x.data, x.name
			usedXML[x.name] = true
		} else if len(xmls) == 1 {
			// Caso típico DIAN: un PDF + un XML con nombres distintos.
			b.XML, b.XMLName = xmls[0].data, xmls[0].name
			usedXML[xmls[0].name] = true
		}
		bundles = append(bundles, b)
	}

	// XML que no quedaron emparejados con ningún PDF: bundle solo-XML.
	for _, x := range xmls {
		if !usedXML[x.name] {
			bundles = append(bundles, Bundle{Origin: origin, XML: x.data, XMLName: x.name})
		}
	}

	if len(bundles) == 0 {
		return nil, fmt.Errorf("el ZIP %q no contiene PDF ni XML", origin)
	}

	// Los demás archivos del ZIP se adjuntan al primer bundle (todos referidos
	// a la misma factura) para insertarlos una sola vez en el Módulo 3.
	for _, o := range others {
		bundles[0].Extras = append(bundles[0].Extras, Extra{
			Name: o.name,
			Ext:  extSinPunto(o.name),
			Data: o.data,
		})
	}
	return bundles, nil
}

// FromPDF crea un bundle con un PDF directo (sin XML de respaldo).
func FromPDF(name string, data []byte) Bundle {
	return Bundle{Origin: name, PDF: data, PDFName: name}
}

// collectDocs recorre las entradas de un ZIP acumulando los PDF y XML en sus
// slices y los demás archivos (imágenes, DOCX, etc.) en others. Si encuentra un
// ZIP anidado desciende recursivamente (depth+1) hasta maxZIPDepth para no caer
// en bucles ni ZIP bombs.
func collectDocs(origin string, data []byte, depth int, pdfs, xmls, others *[]doc) error {
	if depth > maxZIPDepth {
		return fmt.Errorf("ZIP %q excede el máximo de %d niveles de anidamiento", origin, maxZIPDepth)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("no se pudo abrir el ZIP %q: %w", origin, err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		content, err := readZipFile(f)
		if err != nil {
			return fmt.Errorf("error leyendo %q dentro de %q: %w", f.Name, origin, err)
		}
		switch strings.ToLower(filepath.Ext(f.Name)) {
		case ".pdf":
			*pdfs = append(*pdfs, doc{f.Name, content})
		case ".xml":
			*xmls = append(*xmls, doc{f.Name, content})
		case ".zip":
			if err := collectDocs(f.Name, content, depth+1, pdfs, xmls, others); err != nil {
				return err
			}
		default:
			// Cualquier otro archivo del ZIP se adjunta tal cual (JPG, TIF,
			// DOCX, etc.). Sin extensión no hay tipo que registrar: se omite.
			if extSinPunto(f.Name) != "" {
				*others = append(*others, doc{f.Name, content})
			}
		}
	}
	return nil
}

// readZipFile abre y lee por completo una entrada del ZIP.
func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// baseNoExt devuelve el nombre de archivo sin ruta ni extensión, en minúsculas.
func baseNoExt(name string) string {
	b := filepath.Base(name)
	return strings.ToLower(strings.TrimSuffix(b, filepath.Ext(b)))
}

// extSinPunto devuelve la extensión del archivo en minúsculas y sin el punto
// inicial (p.ej. "jpg"); "" si el archivo no tiene extensión.
func extSinPunto(name string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
}
