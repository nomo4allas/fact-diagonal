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
	defaultModel = "gemini-2.5-flash"
	endpointFmt  = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent"
)

// prompt pide a Gemini un JSON estricto con los campos de interés.
const prompt = `Eres un extractor de datos de facturas electrónicas colombianas (DIAN).
Del documento PDF adjunto, devuelve EXCLUSIVAMENTE un objeto JSON válido, sin texto adicional ni bloques de código, con estas claves:
{"numero":"","prefijo":"","nit":"","razon_social":"","fecha_emision":"","valor_total":"","cufe":""}
Reglas:
- "numero": número de la factura tal cual aparece (incluye prefijo si lo tiene).
- "prefijo": prefijo alfabético de la numeración, si existe.
- "nit": NIT del proveedor (emisor).
- "razon_social": razón social del proveedor (emisor).
- "fecha_emision": en formato YYYY-MM-DD.
- "valor_total": valor total a pagar, solo el número.
- "cufe": el código CUFE.
Si un dato no aparece, deja su valor como cadena vacía.`

// Client habla con la API de Gemini.
type Client struct {
	apiKey string
	model  string
	http   *http.Client
}

// New crea un cliente de Gemini. Un apiKey vacío produce un cliente no
// disponible (Available() == false).
func New(apiKey string) *Client {
	return &Client{
		apiKey: strings.TrimSpace(apiKey),
		model:  defaultModel,
		http:   &http.Client{Timeout: 60 * time.Second},
	}
}

// Available indica si hay una clave configurada.
func (c *Client) Available() bool { return c.apiKey != "" }

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
}

// Extract envía el PDF a Gemini y devuelve los campos extraídos.
func (c *Client) Extract(ctx context.Context, pdf []byte) (invoice.Data, error) {
	if !c.Available() {
		return invoice.Data{}, fmt.Errorf("Gemini no disponible: GEMINI_API_KEY vacía")
	}

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

	url := fmt.Sprintf(endpointFmt, c.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return invoice.Data{}, fmt.Errorf("error construyendo la petición a Gemini: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return invoice.Data{}, fmt.Errorf("error llamando a Gemini: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return invoice.Data{}, fmt.Errorf("error leyendo la respuesta de Gemini: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return invoice.Data{}, fmt.Errorf("Gemini respondió %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
	}
	d.DerivePrefijo()
	return d, nil
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
