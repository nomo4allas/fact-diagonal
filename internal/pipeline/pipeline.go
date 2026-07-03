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
	// Procesados: procesamiento exitoso completo → mover a /Procesados.
	Procesados Outcome = iota
	// Pendientes: CUFE no encontrado en BD, 0 adjuntos insertados ("SP devuelve 0"),
	// PDF ilegible o correo sin adjunto de factura válido → /Pendientes.
	Pendientes
	// ErrorTecnico: fallo técnico (llamada al SP, descarga de adjuntos). El correo
	// NO se mueve: queda donde estaba. Mejora 3: ya no existe la carpeta /Errores.
	ErrorTecnico
)

// Folder devuelve el nombre de la subcarpeta de Inbox asociada al desenlace, o
// "" para ErrorTecnico (que no implica movimiento: el correo queda donde estaba).
func (o Outcome) Folder() string {
	switch o {
	case Procesados:
		return "Procesados"
	case Pendientes:
		return "Pendientes"
	default:
		return ""
	}
}

// ErrKind clasifica un fallo técnico para decidir cómo reaccionar (Mejora 1):
// KindSP se notifica a soporte; KindM365 solo se registra en el log local
// (no se puede notificar sin conexión a Graph).
type ErrKind int

const (
	KindNone ErrKind = iota // sin fallo técnico
	KindSP                  // fallo en la llamada al Stored Procedure → notificar
	KindM365                // fallo de conexión a Graph/M365 → solo log local
)

// Report resume el desenlace del procesamiento de un correo para que el llamador
// decida la carpeta destino y, ante un fallo técnico, si notifica a soporte.
type Report struct {
	Results []Result
	Outcome Outcome
	// ErrKind y Err solo son significativos cuando Outcome == ErrorTecnico.
	ErrKind ErrKind
	Err     error
}

// peor devuelve el desenlace de mayor severidad entre dos.
func peor(a, b Outcome) Outcome {
	if b > a {
		return b
	}
	return a
}

// ProcessMessage descarga los adjuntos del correo, extrae los datos de cada
// factura, los registra en el log y (Módulo 3) los persiste. Devuelve un Report
// con los resultados consolidados, el desenlace agregado (Outcome) y, ante un
// fallo técnico, su clasificación (ErrKind) y detalle (Err) para que el llamador
// decida la carpeta destino y si notifica a soporte.
func (p *Processor) ProcessMessage(ctx context.Context, mailbox string, msg graph.Message) Report {
	atts, err := p.graph.ListAttachments(ctx, mailbox, msg.ID)
	if err != nil {
		// Fallo al descargar adjuntos = fallo de conexión a Graph/M365 → solo log
		// local, el correo queda donde estaba (Mejora 1, categoría M365).
		return Report{
			Outcome: ErrorTecnico,
			ErrKind: KindM365,
			Err:     fmt.Errorf("no se pudieron descargar los adjuntos: %w", err),
		}
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
		// Correo sin adjunto de factura válido: no es un fallo técnico, pero no se
		// pudo procesar → Pendientes (queda para revisión/reintento). Mejora 3: ya
		// no existe /Errores.
		p.log.Infof("    · sin adjuntos procesables (ZIP/PDF) en este correo → Pendientes")
		return Report{Outcome: Pendientes}
	}

	var results []Result
	outcome := Procesados
	for _, b := range bundles {
		res, o, err := p.processBundle(ctx, b, msg.ReceivedDateTime)
		results = append(results, res)
		if err != nil {
			// Fallo técnico del Módulo 3 (llamada al SP) → notificar y no mover.
			return Report{Results: results, Outcome: ErrorTecnico, ErrKind: KindSP, Err: err}
		}
		outcome = peor(outcome, o)
	}
	return Report{Results: results, Outcome: outcome}
}

// processBundle extrae los datos de un bundle (un PDF y/o un XML), los logea y,
// si la BD está activa, los persiste (Módulo 3). fechaCorreo es la fecha de
// recepción del correo, usada para FechaHoraOriginal. Devuelve el resultado, el
// desenlace (Outcome) para la clasificación de carpetas y, si hubo un fallo
// técnico del SP, el error (con Outcome == ErrorTecnico) para notificar a soporte.
func (p *Processor) processBundle(ctx context.Context, b attachment.Bundle, fechaCorreo time.Time) (Result, Outcome, error) {
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

	// Si no se pudo extraer ningún campo clave (PDF ilegible / sin datos), no es un
	// fallo técnico de conexión: no se puede hallar el CUFE → Pendientes (revisión/
	// reintento). No se toca la BD. Mejora 3: ya no existe /Errores.
	if res.Final.FilledCount() == 0 {
		p.log.Errorf("    · sin datos aprovechables en el bundle %q (PDF ilegible/vacío) → Pendientes", b.Origin)
		return res, Pendientes, nil
	}

	// 4) Módulo 3: persistir en SQL Server (si está configurado).
	if p.db == nil {
		// Sin Módulo 3 no hay verificación en BD; con la extracción exitosa el
		// correo se considera procesado.
		return res, Procesados, nil
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
		// Fallo técnico en la llamada al SP → ErrorTecnico: el llamador notifica a
		// soporte y deja el correo donde estaba (Mejora 1, categoría SP).
		p.log.Errorf("    · BD: la persistencia falló: %v", err)
		return res, ErrorTecnico, err
	}
	return res, outcomeDeEstado(persist.Estado), nil
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
