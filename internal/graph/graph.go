// Package graph realiza llamadas HTTP directas a Microsoft Graph API
// para leer correos del buzón configurado (solo lectura).
package graph

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const baseURL = "https://graph.microsoft.com/v1.0"

// Client encapsula un *http.Client ya autenticado contra Graph.
type Client struct {
	http *http.Client
}

// New crea un cliente de Graph a partir de un *http.Client autenticado
// (normalmente el devuelto por auth.HTTPClient).
func New(httpClient *http.Client) *Client {
	return &Client{http: httpClient}
}

// EmailAddress representa la dirección de un remitente.
type EmailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// Recipient envuelve una dirección de correo tal como la entrega Graph.
type Recipient struct {
	EmailAddress EmailAddress `json:"emailAddress"`
}

// Message es la representación reducida de un correo, con solo los campos
// que solicitamos vía $select.
type Message struct {
	ID               string     `json:"id"`
	Subject          string     `json:"subject"`
	From             *Recipient `json:"from"`
	ReceivedDateTime time.Time  `json:"receivedDateTime"`
	HasAttachments   bool       `json:"hasAttachments"`
	IsRead           bool       `json:"isRead"`
}

// SenderName devuelve el nombre legible del remitente o, en su defecto,
// la dirección de correo.
func (m Message) SenderName() string {
	if m.From == nil {
		return "(desconocido)"
	}
	if m.From.EmailAddress.Name != "" {
		return fmt.Sprintf("%s <%s>", m.From.EmailAddress.Name, m.From.EmailAddress.Address)
	}
	return m.From.EmailAddress.Address
}

// messagesResponse modela la envoltura de la colección de mensajes,
// incluyendo el enlace de paginación @odata.nextLink.
type messagesResponse struct {
	Value    []Message `json:"value"`
	NextLink string    `json:"@odata.nextLink"`
}

// ListUnreadMessages devuelve todos los correos NO leídos de la bandeja de
// entrada del buzón indicado, siguiendo la paginación de Graph.
//
// Es una operación estrictamente de lectura: no marca como leído ni mueve
// ningún mensaje.
func (c *Client) ListUnreadMessages(ctx context.Context, mailbox string) ([]Message, error) {
	// Construimos la primera URL con el filtro y los campos seleccionados.
	q := url.Values{}
	q.Set("$filter", "isRead eq false")
	q.Set("$select", "id,subject,from,receivedDateTime,hasAttachments,isRead")
	q.Set("$orderby", "receivedDateTime DESC")
	q.Set("$top", "50")

	next := fmt.Sprintf("%s/users/%s/mailFolders/Inbox/messages?%s",
		baseURL, url.PathEscape(mailbox), q.Encode())

	var all []Message
	for next != "" {
		page, err := c.fetchPage(ctx, next)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
		next = page.NextLink
	}
	return all, nil
}

// fetchPage ejecuta una petición GET y decodifica una página de resultados.
func (c *Client) fetchPage(ctx context.Context, rawURL string) (*messagesResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error construyendo la petición: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error llamando a Graph: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error leyendo la respuesta de Graph: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Graph respondió %d: %s", resp.StatusCode, string(body))
	}

	var out messagesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("error decodificando la respuesta de Graph: %w", err)
	}
	return &out, nil
}

// Attachment representa un adjunto de tipo fileAttachment tal como lo entrega
// Graph, con su contenido embebido en base64 (contentBytes).
type Attachment struct {
	ODataType    string `json:"@odata.type"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	Size         int    `json:"size"`
	IsInline     bool   `json:"isInline"`
	ContentBytes string `json:"contentBytes"`
}

// Bytes decodifica el contenido del adjunto desde su representación base64.
func (a Attachment) Bytes() ([]byte, error) {
	if a.ContentBytes == "" {
		return nil, fmt.Errorf("el adjunto %q no trae contentBytes", a.Name)
	}
	data, err := base64.StdEncoding.DecodeString(a.ContentBytes)
	if err != nil {
		return nil, fmt.Errorf("error decodificando base64 del adjunto %q: %w", a.Name, err)
	}
	return data, nil
}

// attachmentsResponse modela la colección de adjuntos de un mensaje.
type attachmentsResponse struct {
	Value []Attachment `json:"value"`
}

// ListAttachments descarga los adjuntos (fileAttachment) de un mensaje,
// incluyendo su contenido en base64. Solo lectura: no modifica el correo.
//
// Graph entrega contentBytes inline hasta cierto tamaño; los adjuntos sin
// contentBytes (p.ej. itemAttachment o referencias) se devuelven igualmente
// para que el llamador decida, pero su .Bytes() fallará.
func (c *Client) ListAttachments(ctx context.Context, mailbox, messageID string) ([]Attachment, error) {
	rawURL := fmt.Sprintf("%s/users/%s/messages/%s/attachments",
		baseURL, url.PathEscape(mailbox), url.PathEscape(messageID))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error construyendo la petición de adjuntos: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error llamando a Graph (adjuntos): %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error leyendo la respuesta de adjuntos: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Graph respondió %d al pedir adjuntos: %s", resp.StatusCode, string(body))
	}

	var out attachmentsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("error decodificando los adjuntos de Graph: %w", err)
	}
	return out.Value, nil
}
