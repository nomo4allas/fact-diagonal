// Package attachment clasifica los adjuntos de un correo y normaliza su
// contenido en "bundles" listos para extraer datos: cada bundle expone, como
// máximo, un PDF y un XML provenientes de la misma factura.
//
// Soporta dos formas de entrega habituales en facturación electrónica DIAN:
//   - Adjunto ZIP que contiene el PDF de representación gráfica y el XML UBL.
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

// FromZIP descomprime el ZIP y devuelve un bundle por cada PDF encontrado,
// emparejándolo con el XML del mismo nombre base si existe; un XML suelto sin
// PDF también genera su propio bundle. Esto cubre el caso típico (un PDF + un
// XML) y los menos comunes (varios documentos en un mismo ZIP).
func FromZIP(origin string, data []byte) ([]Bundle, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("no se pudo abrir el ZIP %q: %w", origin, err)
	}

	type doc struct {
		name string
		data []byte
	}
	var pdfs, xmls []doc

	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Name))
		if ext != ".pdf" && ext != ".xml" {
			continue
		}
		content, err := readZipFile(f)
		if err != nil {
			return nil, fmt.Errorf("error leyendo %q dentro de %q: %w", f.Name, origin, err)
		}
		if ext == ".pdf" {
			pdfs = append(pdfs, doc{f.Name, content})
		} else {
			xmls = append(xmls, doc{f.Name, content})
		}
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
	return bundles, nil
}

// FromPDF crea un bundle con un PDF directo (sin XML de respaldo).
func FromPDF(name string, data []byte) Bundle {
	return Bundle{Origin: name, PDF: data, PDFName: name}
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
