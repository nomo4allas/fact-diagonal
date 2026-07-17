// Package gemini es el último eslabón de la cascada: envía el PDF a la API de
// Gemini y le pide los campos de la factura en JSON. Útil cuando ni el texto
// nativo ni el OCR local logran extraer la información.
//
// Se implementa contra la API REST (generativelanguage) sin SDK para no añadir
// dependencias. Si no hay clave configurada, Available() devuelve false y la
// cascada omite este eslabón.
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nomo4allas/fact-diagonal/internal/invoice"
)

const (
	// Valores por defecto si el llamador no configura modelo/location. El modelo
	// se lee de config.env (GEMINI_MODEL); gemini-2.5-flash fue deprecado por
	// Google (404), por eso ya no se cablea aquí.
	defaultModel    = "gemini-2.0-flash"
	defaultLocation = "global"
	endpointFmt     = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent"

	// La API devuelve 503/429 transitorios bajo alta demanda; reintentamos con
	// backoff exponencial antes de rendirnos.
	maxAttempts = 4
	baseBackoff = 1500 * time.Millisecond

	// callTimeout acota cada llamada completa a Gemini (incluidos los reintentos).
	// Mejora 1: cada PDF usa su propio contexto con este límite, independiente del
	// contexto global de la corrida, para que un cuelgue o fallo de Gemini quede
	// contenido y no afecte las llamadas posteriores a SQL Server ni a MS365.
	callTimeout = 90 * time.Second
)

// prompt pide a Gemini un JSON estricto con los campos de interés.
const prompt = `Eres un extractor de datos de facturas electrónicas colombianas (DIAN).
Del documento PDF adjunto, devuelve EXCLUSIVAMENTE un objeto JSON válido, sin texto adicional ni bloques de código, con estas claves:
{"numero":"","prefijo":"","nit":"","razon_social":"","fecha_emision":"","valor_total":"","cufe":"","pedido":"","declarac":"","bl":""}
Reglas:
- "numero": número de la factura tal cual aparece (incluye prefijo si lo tiene).
- "prefijo": prefijo alfabético de la numeración, si existe.
- "nit": NIT del proveedor (emisor).
- "razon_social": razón social del proveedor (emisor).
- "fecha_emision": en formato YYYY-MM-DD.
- "valor_total": valor total a pagar, solo el número.
- "cufe": el código CUFE.
- "pedido": el número de pedido, suele aparecer como "PEDIDO No:".
- "declarac": el valor de "DECLARAC:".
- "bl": el Bill of Lading, aparece como "DOCTTE:" o "N° BL:" según el proveedor.
Si un dato no aparece, deja su valor como cadena vacía.`

// Client habla con la API de Gemini.
type Client struct {
	apiKey   string
	model    string
	location string
	http     *http.Client
}

// New crea un cliente de Gemini con el modelo y la location indicados (leídos de
// config.env: GEMINI_MODEL y GEMINI_LOCATION). Si alguno viene vacío se usa el
// valor por defecto. Un apiKey vacío produce un cliente no disponible
// (Available() == false).
func New(apiKey, model, location string) *Client {
	return &Client{
		apiKey:   strings.TrimSpace(apiKey),
		model:    orDefault(model, defaultModel),
		location: orDefault(location, defaultLocation),
		http:     &http.Client{Timeout: 60 * time.Second},
	}
}

// orDefault devuelve v sin espacios, o def si v viene vacío.
func orDefault(v, def string) string {
	if s := strings.TrimSpace(v); s != "" {
		return s
	}
	return def
}

// Available indica si hay una clave configurada.
func (c *Client) Available() bool { return c.apiKey != "" }

// Describe devuelve el modelo y la location configurados, para registrarlos en
// el log al habilitar la cascada.
func (c *Client) Describe() string {
	return fmt.Sprintf("modelo=%s, location=%s", c.model, c.location)
}

// ---- Estructuras de la petición/respuesta REST ----

type genRequest struct {
	Contents []content `json:"contents"`
}

type content struct {
	Parts []part `json:"parts"`
}

type part struct {
	Text       string      `json:"text,omitempty"`
	InlineData *inlineData `json:"inline_data,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

type genResponse struct {
	Candidates []struct {
		Content content `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// fields es el JSON que esperamos de vuelta del modelo.
type fields struct {
	Numero       string `json:"numero"`
	Prefijo      string `json:"prefijo"`
	NIT          string `json:"nit"`
	RazonSocial  string `json:"razon_social"`
	FechaEmision string `json:"fecha_emision"`
	ValorTotal   string `json:"valor_total"`
	CUFE         string `json:"cufe"`
	// Ajuste Módulo 2 — campos adicionales del PDF.
	Pedido   string `json:"pedido"`
	Declarac string `json:"declarac"`
	BL       string `json:"bl"`
}

// Extract envía el PDF a Gemini y devuelve los campos extraídos.
//
// Mejora 1: la llamada usa su propio context.WithTimeout (callTimeout), derivado
// de context.Background() y NO del ctx global de la corrida. Así, si Gemini se
// cuelga o falla, el error queda contenido en esta llamada y no propaga la
// cancelación al resto del pipeline (SQL Server, MS365). El ctx recibido se
// conserva en la firma por compatibilidad con la cascada, pero deliberadamente
// no se encadena para lograr esa independencia.
func (c *Client) Extract(_ context.Context, pdf []byte) (invoice.Data, error) {
	if !c.Available() {
		return invoice.Data{}, fmt.Errorf("Gemini no disponible: GEMINI_API_KEY vacía")
	}

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	reqBody := genRequest{
		Contents: []content{{
			Parts: []part{
				{Text: prompt},
				{InlineData: &inlineData{
					MimeType: "application/pdf",
					Data:     base64.StdEncoding.EncodeToString(pdf),
				}},
			},
		}},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return invoice.Data{}, fmt.Errorf("error serializando la petición a Gemini: %w", err)
	}

	body, err := c.doWithRetry(ctx, payload)
	if err != nil {
		return invoice.Data{}, err
	}

	var gr genResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return invoice.Data{}, fmt.Errorf("error decodificando la respuesta de Gemini: %w", err)
	}
	if gr.Error != nil {
		return invoice.Data{}, fmt.Errorf("Gemini devolvió error: %s", gr.Error.Message)
	}

	raw := firstText(gr)
	if raw == "" {
		return invoice.Data{}, fmt.Errorf("Gemini no devolvió contenido")
	}

	var f fields
	if err := json.Unmarshal([]byte(extractJSON(raw)), &f); err != nil {
		return invoice.Data{}, fmt.Errorf("la respuesta de Gemini no es JSON válido: %w (texto: %q)", err, raw)
	}

	d := invoice.Data{
		Numero:       strings.TrimSpace(f.Numero),
		Prefijo:      strings.TrimSpace(f.Prefijo),
		NIT:          strings.TrimSpace(f.NIT),
		RazonSocial:  strings.TrimSpace(f.RazonSocial),
		FechaEmision: strings.TrimSpace(f.FechaEmision),
		ValorTotal:   strings.TrimSpace(f.ValorTotal),
		CUFE:         strings.TrimSpace(f.CUFE),
		Source:       invoice.SourceGemini,
		// Ajuste Módulo 2 — campos adicionales del PDF.
		Pedido:   strings.TrimSpace(f.Pedido),
		Declarac: strings.TrimSpace(f.Declarac),
		BL:       strings.TrimSpace(f.BL),
	}
	d.DerivePrefijo()
	return d, nil
}

// doWithRetry envía la petición y reintenta ante errores transitorios
// (503/429/500) con backoff exponencial. Devuelve el cuerpo de la respuesta
// 200 OK o el último error.
func (c *Client) doWithRetry(ctx context.Context, payload []byte) ([]byte, error) {
	url := fmt.Sprintf(endpointFmt, c.model)
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("error construyendo la petición a Gemini: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", c.apiKey)

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("error llamando a Gemini: %w", err)
		} else {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			switch {
			case readErr != nil:
				lastErr = fmt.Errorf("error leyendo la respuesta de Gemini: %w", readErr)
			case resp.StatusCode == http.StatusOK:
				return body, nil
			case isTransient(resp.StatusCode):
				lastErr = fmt.Errorf("Gemini respondió %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			default:
				// Error no recuperable (p.ej. 400/401/403): no reintentamos.
				return nil, fmt.Errorf("Gemini respondió %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			}
		}

		if attempt < maxAttempts {
			// Backoff exponencial: 1.5s, 3s, 6s…
			wait := baseBackoff * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	return nil, fmt.Errorf("Gemini sin éxito tras %d intentos: %w", maxAttempts, lastErr)
}

// isTransient indica si conviene reintentar para el código de estado dado.
func isTransient(status int) bool {
	switch status {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

func firstText(gr genResponse) string {
	for _, cand := range gr.Candidates {
		for _, p := range cand.Content.Parts {
			if p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

// reJSON aísla el primer objeto JSON dentro de un texto que pudiera traer
// envoltura (p.ej. ```json … ```).
var reJSON = regexp.MustCompile(`(?s)\{.*\}`)

func extractJSON(s string) string {
	if m := reJSON.FindString(s); m != "" {
		return m
	}
	return s
}
