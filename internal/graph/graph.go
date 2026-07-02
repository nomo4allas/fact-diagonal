// Package graph realiza llamadas HTTP directas a Microsoft Graph API
// para leer correos del buzón configurado (solo lectura).
package graph

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

// ---------------------------------------------------------------------------
// Manejo de carpetas del buzón (ajuste "lógica de carpetas").
//
// Las carpetas destino (Procesados/Pendientes/Errores) son SUBCARPETAS de Inbox.
// ResolveInboxChildFolder las localiza por displayName y las crea si no existen;
// MoveMessage mueve un correo a una carpeta. Ambas operaciones ESCRIBEN en el
// buzón, por lo que el llamador debe respetar SIMULATION_MODE (no invocarlas en
// simulación, solo registrar en el log lo que haría).
// ---------------------------------------------------------------------------

// mailFolder es la representación reducida de una carpeta de correo.
type mailFolder struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type mailFoldersResponse struct {
	Value    []mailFolder `json:"value"`
	NextLink string       `json:"@odata.nextLink"`
}

// ResolveInboxChildFolder devuelve el ID de la subcarpeta de Inbox con el
// displayName indicado, creándola si no existe. La comparación de nombre es
// case-insensitive.
func (c *Client) ResolveInboxChildFolder(ctx context.Context, mailbox, displayName string) (string, error) {
	id, found, err := c.findInboxChildFolder(ctx, mailbox, displayName)
	if err != nil {
		return "", err
	}
	if found {
		return id, nil
	}
	return c.createInboxChildFolder(ctx, mailbox, displayName)
}

// findInboxChildFolder busca (siguiendo la paginación) una subcarpeta de Inbox
// por displayName.
func (c *Client) findInboxChildFolder(ctx context.Context, mailbox, displayName string) (string, bool, error) {
	q := url.Values{}
	q.Set("$select", "id,displayName")
	q.Set("$top", "100")
	next := fmt.Sprintf("%s/users/%s/mailFolders/Inbox/childFolders?%s",
		baseURL, url.PathEscape(mailbox), q.Encode())

	for next != "" {
		body, err := c.doGET(ctx, next)
		if err != nil {
			return "", false, fmt.Errorf("error listando subcarpetas de Inbox: %w", err)
		}
		var out mailFoldersResponse
		if err := json.Unmarshal(body, &out); err != nil {
			return "", false, fmt.Errorf("error decodificando subcarpetas de Inbox: %w", err)
		}
		for _, f := range out.Value {
			if strings.EqualFold(f.DisplayName, displayName) {
				return f.ID, true, nil
			}
		}
		next = out.NextLink
	}
	return "", false, nil
}

// createInboxChildFolder crea una subcarpeta bajo Inbox y devuelve su ID.
func (c *Client) createInboxChildFolder(ctx context.Context, mailbox, displayName string) (string, error) {
	rawURL := fmt.Sprintf("%s/users/%s/mailFolders/Inbox/childFolders",
		baseURL, url.PathEscape(mailbox))
	payload, err := json.Marshal(map[string]string{"displayName": displayName})
	if err != nil {
		return "", fmt.Errorf("error serializando la creación de carpeta %q: %w", displayName, err)
	}
	body, err := c.doJSON(ctx, http.MethodPost, rawURL, payload, http.StatusCreated, http.StatusOK)
	if err != nil {
		return "", fmt.Errorf("error creando la subcarpeta %q: %w", displayName, err)
	}
	var f mailFolder
	if err := json.Unmarshal(body, &f); err != nil {
		return "", fmt.Errorf("error decodificando la carpeta creada %q: %w", displayName, err)
	}
	return f.ID, nil
}

// MoveMessage mueve un correo a la carpeta destino indicada por su ID. Graph
// devuelve el mensaje ya movido (con un nuevo id) en la carpeta destino.
func (c *Client) MoveMessage(ctx context.Context, mailbox, messageID, destFolderID string) error {
	rawURL := fmt.Sprintf("%s/users/%s/messages/%s/move",
		baseURL, url.PathEscape(mailbox), url.PathEscape(messageID))
	payload, err := json.Marshal(map[string]string{"destinationId": destFolderID})
	if err != nil {
		return fmt.Errorf("error serializando el move del correo: %w", err)
	}
	if _, err := c.doJSON(ctx, http.MethodPost, rawURL, payload, http.StatusCreated, http.StatusOK); err != nil {
		return fmt.Errorf("error moviendo el correo a la carpeta destino: %w", err)
	}
	return nil
}

// doGET ejecuta un GET autenticado y devuelve el cuerpo si la respuesta es 200.
func (c *Client) doGET(ctx context.Context, rawURL string) ([]byte, error) {
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
	return body, nil
}

// doJSON ejecuta una petición con cuerpo JSON y devuelve el cuerpo de la
// respuesta si su código está entre los okStatus indicados.
func (c *Client) doJSON(ctx context.Context, method, rawURL string, payload []byte, okStatus ...int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("error construyendo la petición: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
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
	for _, ok := range okStatus {
		if resp.StatusCode == ok {
			return body, nil
		}
	}
	return nil, fmt.Errorf("Graph respondió %d: %s", resp.StatusCode, string(body))
}
