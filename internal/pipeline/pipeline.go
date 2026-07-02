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
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/attachment"
	"github.com/nomo4allas/fact-diagonal/internal/database"
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
	db         *database.Client // Módulo 3; nil si la BD no está configurada
	log        Logger
	simulation bool
}

// New construye el Processor de los Módulos 2 y 3. db puede ser nil para
// desactivar la integración con SQL Server.
func New(gc *graph.Client, gem *gemini.Client, db *database.Client, log Logger, simulation bool) *Processor {
	return &Processor{
		graph:      gc,
		ocr:        ocr.New(),
		gemini:     gem,
		db:         db,
		log:        log,
		simulation: simulation,
	}
}

// Result reúne lo extraído de un bundle: los datos por fuente y el consolidado.
type Result struct {
	Origin  string       // adjunto de origen
	XMLData invoice.Data // datos del XML (vacío si no había XML)
	PDFData invoice.Data // mejor resultado de la cascada de PDF
	Final   invoice.Data // consolidado (XML autoritativo + respaldo del PDF)
}

// Outcome clasifica el desenlace de un correo para decidir su carpeta destino
// (ajuste "lógica de carpetas"). El valor entero codifica la severidad: al
// agregar varios bundles de un mismo correo se conserva el de mayor severidad.
type Outcome int

const (
	// Procesados: procesamiento exitoso completo.
	Procesados Outcome = iota
	// Pendientes: CUFE no encontrado en BD, o 0 adjuntos insertados ("SP devuelve 0").
	Pendientes
	// Errores: error técnico (PDF ilegible, fallo de conexión, etc.) o correo sin
	// adjunto de factura válido.
	Errores
)

// Folder devuelve el nombre de la subcarpeta de Inbox asociada al desenlace.
func (o Outcome) Folder() string {
	switch o {
	case Procesados:
		return "Procesados"
	case Pendientes:
		return "Pendientes"
	default:
		return "Errores"
	}
}

// peor devuelve el desenlace de mayor severidad entre dos.
func peor(a, b Outcome) Outcome {
	if b > a {
		return b
	}
	return a
}

// ProcessMessage descarga los adjuntos del correo, extrae los datos de cada
// factura, los registra en el log y (Módulo 3) los persiste. Devuelve los
// resultados consolidados y el desenlace agregado del correo (Outcome), que el
// llamador usa para decidir la carpeta destino.
func (p *Processor) ProcessMessage(ctx context.Context, mailbox string, msg graph.Message) ([]Result, Outcome, error) {
	atts, err := p.graph.ListAttachments(ctx, mailbox, msg.ID)
	if err != nil {
		// Fallo técnico al descargar adjuntos → Errores.
		return nil, Errores, fmt.Errorf("no se pudieron descargar los adjuntos: %w", err)
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
		// Correo sin adjunto de factura válido → Errores.
		p.log.Infof("    · sin adjuntos procesables (ZIP/PDF) en este correo")
		return nil, Errores, nil
	}

	var results []Result
	outcome := Procesados
	for _, b := range bundles {
		res, o := p.processBundle(ctx, b, msg.ReceivedDateTime)
		results = append(results, res)
		outcome = peor(outcome, o)
	}
	return results, outcome, nil
}

// processBundle extrae los datos de un bundle (un PDF y/o un XML), los logea y,
// si la BD está activa, los persiste (Módulo 3). fechaCorreo es la fecha de
// recepción del correo, usada para FechaHoraOriginal. Devuelve, además del
// resultado, el desenlace (Outcome) para la clasificación de carpetas.
func (p *Processor) processBundle(ctx context.Context, b attachment.Bundle, fechaCorreo time.Time) (Result, Outcome) {
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

	// Si no se pudo extraer ningún campo clave, tratamos el bundle como error
	// técnico (PDF ilegible / sin datos aprovechables) → Errores. No se toca la BD.
	if res.Final.FilledCount() == 0 {
		p.log.Errorf("    · sin datos aprovechables en el bundle %q (PDF ilegible/vacío) → Errores", b.Origin)
		return res, Errores
	}

	// 4) Módulo 3: persistir en SQL Server (si está configurado).
	if p.db == nil {
		// Sin Módulo 3 no hay verificación en BD; con la extracción exitosa el
		// correo se considera procesado.
		return res, Procesados
	}

	var adjuntos []database.Adjunto
	if b.HasPDF() {
		adjuntos = append(adjuntos, database.Adjunto{Nombre: b.PDFName, Extension: "pdf", Contenido: b.PDF})
	}
	if b.HasXML() {
		adjuntos = append(adjuntos, database.Adjunto{Nombre: b.XMLName, Extension: "xml", Contenido: b.XML})
	}
	persist, err := p.db.PersistInvoice(ctx, res.Final, fechaCorreo, adjuntos)
	if err != nil {
		// Fallo técnico de BD (p.ej. conexión) → Errores.
		p.log.Errorf("    · BD: la persistencia falló: %v", err)
		return res, Errores
	}
	return res, outcomeDeEstado(persist.Estado)
}

// outcomeDeEstado traduce el desenlace de la BD (Módulo 3) a la carpeta destino.
func outcomeDeEstado(e database.EstadoBD) Outcome {
	switch e {
	case database.EstadoProcesado:
		return Procesados
	default: // EstadoNoHallado (CUFE no encontrado) o EstadoPendiente (0 adjuntos)
		return Pendientes
	}
}

// cascadePDF aplica los tres eslabones en orden, deteniéndose en cuanto un
// eslabón devuelve un resultado completo. Conserva el resultado más rico.
func (p *Processor) cascadePDF(ctx context.Context, pdf []byte) (invoice.Data, []string) {
	var notes []string
	best := invoice.Data{}
	// Los campos adicionales del PDF (Pedido/Declarac/BL) no cuentan para
	// IsComplete()/richer, así que los acumulamos aparte para que no se pierdan
	// cuando un eslabón posterior, más rico en campos clave, reemplace a "best".
	//
	// Prioridad de fuentes (ajuste): Gemini > OCR > texto nativo. Como los eslabones
	// corren en ese orden inverso de confianza (nativo → OCR → Gemini), basta con
	// que gane el ÚLTIMO valor no vacío: así el regex heurístico del texto nativo es
	// solo respaldo y Gemini (más fiable) tiene la última palabra.
	var extras invoice.Data

	acumularExtras := func(d invoice.Data) {
		if strings.TrimSpace(d.Pedido) != "" {
			extras.Pedido = d.Pedido
		}
		if strings.TrimSpace(d.Declarac) != "" {
			extras.Declarac = d.Declarac
		}
		if strings.TrimSpace(d.BL) != "" {
			extras.BL = d.BL
		}
	}
	aplicarExtras := func(d invoice.Data) invoice.Data {
		if strings.TrimSpace(d.Pedido) == "" {
			d.Pedido = extras.Pedido
		}
		if strings.TrimSpace(d.Declarac) == "" {
			d.Declarac = extras.Declarac
		}
		if strings.TrimSpace(d.BL) == "" {
			d.BL = extras.BL
		}
		return d
	}

	// Eslabón 1: texto nativo.
	if d, _, err := pdftext.Extract(pdf); err != nil {
		notes = append(notes, "texto nativo omitido: "+err.Error())
	} else {
		notes = append(notes, fmt.Sprintf("texto nativo: %d/6 campos", d.FilledCount()))
		acumularExtras(d)
		best = richer(best, d)
		if best.IsComplete() {
			return aplicarExtras(best), notes
		}
	}

	// Eslabón 2: OCR con Tesseract.
	if p.ocr.Available() {
		if d, _, err := p.ocr.Extract(ctx, pdf); err != nil {
			notes = append(notes, "OCR falló: "+err.Error())
		} else {
			notes = append(notes, fmt.Sprintf("OCR: %d/6 campos", d.FilledCount()))
			acumularExtras(d)
			best = richer(best, d)
			if best.IsComplete() {
				return aplicarExtras(best), notes
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
			acumularExtras(d)
			best = richer(best, d)
		}
	} else {
		notes = append(notes, "Gemini omitido (GEMINI_API_KEY vacía)")
	}

	return aplicarExtras(best), notes
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
	// Campos adicionales del PDF (ajuste Módulo 2).
	p.log.Infof("       Pedido         : %s", orNA(d.Pedido))
	p.log.Infof("       DECLARAC       : %s", orNA(d.Declarac))
	p.log.Infof("       BL             : %s", orNA(d.BL))
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
