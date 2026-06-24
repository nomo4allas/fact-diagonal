// Package pipeline orquesta el Módulo 2: por cada correo con adjuntos descarga
// los ZIP/PDF, los normaliza en bundles (PDF + XML) y extrae los campos de la
// factura aplicando la cascada texto-nativo → Tesseract → Gemini sobre el PDF,
// con el XML UBL como fuente autoritativa/validación.
//
// Respeta el MODO SIMULACIÓN: no mueve ni marca correos; solo lee, extrae y
// registra los resultados en el log.
package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/nomo4allas/fact-diagonal/internal/attachment"
	"github.com/nomo4allas/fact-diagonal/internal/extract/gemini"
	"github.com/nomo4allas/fact-diagonal/internal/extract/ocr"
	"github.com/nomo4allas/fact-diagonal/internal/extract/pdftext"
	"github.com/nomo4allas/fact-diagonal/internal/extract/xmldoc"
	"github.com/nomo4allas/fact-diagonal/internal/graph"
	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

// Logger es el subconjunto del logger que necesita el pipeline.
type Logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// Processor agrupa las dependencias para procesar los adjuntos de un correo.
type Processor struct {
	graph      *graph.Client
	ocr        *ocr.Engine
	gemini     *gemini.Client
	log        Logger
	simulation bool
}

// New construye el Processor del Módulo 2.
func New(gc *graph.Client, gem *gemini.Client, log Logger, simulation bool) *Processor {
	return &Processor{
		graph:      gc,
		ocr:        ocr.New(),
		gemini:     gem,
		log:        log,
		simulation: simulation,
	}
}

// Result reúne lo extraído de un bundle: los datos por fuente y el consolidado.
type Result struct {
	Origin string       // adjunto de origen
	XMLData invoice.Data // datos del XML (vacío si no había XML)
	PDFData invoice.Data // mejor resultado de la cascada de PDF
	Final   invoice.Data // consolidado (XML autoritativo + respaldo del PDF)
}

// ProcessMessage descarga los adjuntos del correo, extrae los datos de cada
// factura y los registra en el log. Devuelve los resultados consolidados.
func (p *Processor) ProcessMessage(ctx context.Context, mailbox string, msg graph.Message) ([]Result, error) {
	atts, err := p.graph.ListAttachments(ctx, mailbox, msg.ID)
	if err != nil {
		return nil, fmt.Errorf("no se pudieron descargar los adjuntos: %w", err)
	}

	var bundles []attachment.Bundle
	for _, a := range atts {
		if !attachment.IsRelevant(a.Name, a.ContentType) {
			p.log.Infof("    · adjunto ignorado (no es ZIP/PDF): %s", a.Name)
			continue
		}
		data, err := a.Bytes()
		if err != nil {
			p.log.Errorf("    · no se pudo leer el adjunto %s: %v", a.Name, err)
			continue
		}

		switch {
		case attachment.IsZIP(a.Name, a.ContentType):
			bs, err := attachment.FromZIP(a.Name, data)
			if err != nil {
				p.log.Errorf("    · ZIP inválido %s: %v", a.Name, err)
				continue
			}
			p.log.Infof("    · ZIP %s → %d documento(s) interno(s)", a.Name, len(bs))
			bundles = append(bundles, bs...)
		case attachment.IsPDF(a.Name, a.ContentType):
			p.log.Infof("    · PDF directo %s", a.Name)
			bundles = append(bundles, attachment.FromPDF(a.Name, data))
		}
	}

	if len(bundles) == 0 {
		p.log.Infof("    · sin adjuntos procesables (ZIP/PDF) en este correo")
		return nil, nil
	}

	var results []Result
	for _, b := range bundles {
		results = append(results, p.processBundle(ctx, b))
	}
	return results, nil
}

// processBundle extrae los datos de un bundle (un PDF y/o un XML) y los logea.
func (p *Processor) processBundle(ctx context.Context, b attachment.Bundle) Result {
	res := Result{Origin: b.Origin}

	// 1) XML UBL: fuente autoritativa.
	if b.HasXML() {
		d, err := xmldoc.Parse(b.XML)
		if err != nil {
			p.log.Errorf("    · XML %s no se pudo parsear: %v", b.XMLName, err)
		} else {
			res.XMLData = d
			p.log.Infof("    · XML %s: %d/6 campos extraídos", b.XMLName, d.FilledCount())
		}
	}

	// 2) PDF: cascada de extracción (respaldo/validación).
	if b.HasPDF() {
		d, notes := p.cascadePDF(ctx, b.PDF)
		res.PDFData = d
		for _, n := range notes {
			p.log.Infof("    · cascada PDF %s — %s", b.PDFName, n)
		}
	}

	// 3) Consolidación: XML manda; el PDF rellena lo que falte.
	res.Final = consolidate(res.XMLData, res.PDFData)

	p.logDiscrepancies(res)
	p.logFinal(res.Final)
	return res
}

// cascadePDF aplica los tres eslabones en orden, deteniéndose en cuanto un
// eslabón devuelve un resultado completo. Conserva el resultado más rico.
func (p *Processor) cascadePDF(ctx context.Context, pdf []byte) (invoice.Data, []string) {
	var notes []string
	best := invoice.Data{}

	// Eslabón 1: texto nativo.
	if d, _, err := pdftext.Extract(pdf); err != nil {
		notes = append(notes, "texto nativo omitido: "+err.Error())
	} else {
		notes = append(notes, fmt.Sprintf("texto nativo: %d/6 campos", d.FilledCount()))
		best = richer(best, d)
		if best.IsComplete() {
			return best, notes
		}
	}

	// Eslabón 2: OCR con Tesseract.
	if p.ocr.Available() {
		if d, _, err := p.ocr.Extract(ctx, pdf); err != nil {
			notes = append(notes, "OCR falló: "+err.Error())
		} else {
			notes = append(notes, fmt.Sprintf("OCR: %d/6 campos", d.FilledCount()))
			best = richer(best, d)
			if best.IsComplete() {
				return best, notes
			}
		}
	} else {
		notes = append(notes, "OCR omitido (faltan binarios: "+strings.Join(p.ocr.MissingTools(), ", ")+")")
	}

	// Eslabón 3: Gemini.
	if p.gemini.Available() {
		if d, err := p.gemini.Extract(ctx, pdf); err != nil {
			notes = append(notes, "Gemini falló: "+err.Error())
		} else {
			notes = append(notes, fmt.Sprintf("Gemini: %d/6 campos", d.FilledCount()))
			best = richer(best, d)
		}
	} else {
		notes = append(notes, "Gemini omitido (GEMINI_API_KEY vacía)")
	}

	return best, notes
}

// consolidate combina XML (autoritativo) y PDF (respaldo). Si no hay XML,
// devuelve directamente el PDF.
func consolidate(xmlData, pdfData invoice.Data) invoice.Data {
	switch {
	case xmlData.FilledCount() == 0:
		return pdfData
	case pdfData.FilledCount() == 0:
		return xmlData
	default:
		return xmlData.Merge(pdfData)
	}
}

// logDiscrepancies avisa cuando XML y PDF difieren en un mismo campo, lo que
// puede señalar un problema de lectura o una factura adulterada.
func (p *Processor) logDiscrepancies(r Result) {
	if r.XMLData.FilledCount() == 0 || r.PDFData.FilledCount() == 0 {
		return
	}
	type campo struct{ nombre, xml, pdf string }
	campos := []campo{
		{"numero", r.XMLData.Numero, r.PDFData.Numero},
		{"nit", r.XMLData.NIT, r.PDFData.NIT},
		{"cufe", r.XMLData.CUFE, r.PDFData.CUFE},
		{"valor_total", r.XMLData.ValorTotal, r.PDFData.ValorTotal},
		{"fecha", r.XMLData.FechaEmision, r.PDFData.FechaEmision},
	}
	for _, c := range campos {
		x, pdf := strings.TrimSpace(c.xml), strings.TrimSpace(c.pdf)
		if x != "" && pdf != "" && !strings.EqualFold(x, pdf) {
			p.log.Infof("    · ⚠ discrepancia en %s: XML=%q vs PDF=%q", c.nombre, x, pdf)
		}
	}
}

// logFinal vuelca los campos consolidados de la factura.
func (p *Processor) logFinal(d invoice.Data) {
	p.log.Infof("    ── Campos extraídos (consolidado) ──")
	p.log.Infof("       Número factura : %s", orNA(d.Numero))
	p.log.Infof("       Prefijo        : %s", orNA(d.Prefijo))
	p.log.Infof("       NIT proveedor  : %s", orNA(d.NIT))
	p.log.Infof("       Razón social   : %s", orNA(d.RazonSocial))
	p.log.Infof("       Fecha emisión  : %s", orNA(d.FechaEmision))
	p.log.Infof("       Valor total    : %s", orNA(d.ValorTotal))
	p.log.Infof("       CUFE           : %s", orNA(d.CUFE))
	if miss := d.MissingFields(); len(miss) > 0 {
		p.log.Infof("       (incompleto, faltan: %s)", strings.Join(miss, ", "))
	}
}

// richer devuelve el resultado con más campos clave completos; ante empate
// conserva el actual (a).
func richer(a, b invoice.Data) invoice.Data {
	if b.FilledCount() > a.FilledCount() {
		return b
	}
	return a
}

func orNA(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no disponible)"
	}
	return s
}
