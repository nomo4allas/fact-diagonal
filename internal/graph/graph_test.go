package graph

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// servidorFake levanta un Graph de mentira que responde a cada ruta con el JSON
// indicado y registra las rutas pedidas. Devuelve el cliente y un puntero a las
// rutas registradas.
func servidorFake(t *testing.T, rutas map[string]string) (*Client, *[]string) {
	t.Helper()
	var pedidas []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pedidas = append(pedidas, r.URL.Path)
		cuerpo, ok := rutas[r.URL.Path]
		if !ok {
			http.Error(w, `{"error":"ruta no esperada: `+r.URL.Path+`"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(cuerpo))
	}))
	t.Cleanup(srv.Close)

	// baseURL es una constante del paquete; apuntamos el cliente al servidor de
	// prueba sustituyéndola en las URLs mediante un http.Client con Transport que
	// reescribe el host.
	c := &Client{http: &http.Client{Transport: redirigir{destino: srv.URL, base: srv.Client().Transport}}}
	return c, &pedidas
}

// redirigir reescribe el host de graph.microsoft.com al del servidor de prueba,
// conservando la ruta: así verificamos las URLs que construye el paquete sin
// tocar la constante baseURL.
type redirigir struct {
	destino string
	base    http.RoundTripper
}

func (t redirigir) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.String(), baseURL) {
		nuevo := strings.Replace(req.URL.String(), baseURL, t.destino, 1)
		u, err := req.URL.Parse(nuevo)
		if err != nil {
			return nil, err
		}
		req = req.Clone(req.Context())
		req.URL = u
		req.Host = u.Host
	}
	rt := t.base
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(req)
}

const sinMensajes = `{"value":[]}`

// TestListUnreadMessagesUsaLaCarpetaIndicada comprueba que la carpeta de entrada
// es un parámetro y no "Inbox" fijo: con InboxWellKnown se lee de la Bandeja de
// entrada y con un folderID se lee de esa carpeta.
func TestListUnreadMessagesUsaLaCarpetaIndicada(t *testing.T) {
	casos := []struct {
		nombre, folderID, rutaEsperada string
	}{
		{"bandeja de entrada por defecto", InboxWellKnown, "/users/buzon@empresa.com/mailFolders/Inbox/messages"},
		{"carpeta de INBOX_FOLDER", "AAA123", "/users/buzon@empresa.com/mailFolders/AAA123/messages"},
	}
	for _, k := range casos {
		t.Run(k.nombre, func(t *testing.T) {
			c, pedidas := servidorFake(t, map[string]string{k.rutaEsperada: sinMensajes})

			if _, err := c.ListUnreadMessages(context.Background(), "buzon@empresa.com", k.folderID); err != nil {
				t.Fatalf("ListUnreadMessages devolvió error: %v", err)
			}
			if len(*pedidas) != 1 || (*pedidas)[0] != k.rutaEsperada {
				t.Errorf("rutas pedidas = %v, want [%s]", *pedidas, k.rutaEsperada)
			}
		})
	}
}

// TestFindChildFolderCuelgaDelPadreIndicado comprueba que /Pendientes se busca
// como hija de la carpeta de entrada, no de Inbox fijo.
func TestFindChildFolderCuelgaDelPadreIndicado(t *testing.T) {
	const ruta = "/users/buzon@empresa.com/mailFolders/AAA123/childFolders"
	c, pedidas := servidorFake(t, map[string]string{
		ruta: `{"value":[{"id":"PEND1","displayName":"Pendientes"}]}`,
	})

	id, found, err := c.FindChildFolder(context.Background(), "buzon@empresa.com", "AAA123", "Pendientes")
	if err != nil {
		t.Fatalf("FindChildFolder devolvió error: %v", err)
	}
	if !found || id != "PEND1" {
		t.Errorf("FindChildFolder = (%q,%v), want (\"PEND1\",true)", id, found)
	}
	if len(*pedidas) != 1 || (*pedidas)[0] != ruta {
		t.Errorf("rutas pedidas = %v, want [%s]", *pedidas, ruta)
	}
}

// TestFindInboxFolderPrimerNivel: una carpeta como "Pruebas" creada al mismo
// nivel que la Bandeja de entrada se encuentra sin consultar las hijas de Inbox.
func TestFindInboxFolderPrimerNivel(t *testing.T) {
	const raiz = "/users/buzon@empresa.com/mailFolders"
	c, pedidas := servidorFake(t, map[string]string{
		raiz: `{"value":[{"id":"INB","displayName":"Bandeja de entrada"},{"id":"PRU1","displayName":"Pruebas"}]}`,
	})

	id, found, err := c.FindInboxFolder(context.Background(), "buzon@empresa.com", "Pruebas")
	if err != nil {
		t.Fatalf("FindInboxFolder devolvió error: %v", err)
	}
	if !found || id != "PRU1" {
		t.Errorf("FindInboxFolder = (%q,%v), want (\"PRU1\",true)", id, found)
	}
	if len(*pedidas) != 1 {
		t.Errorf("se esperaba una sola consulta (primer nivel), rutas = %v", *pedidas)
	}
}

// TestFindInboxFolderHijaDeInbox: si no está en el primer nivel, se busca entre
// las subcarpetas de la Bandeja de entrada.
func TestFindInboxFolderHijaDeInbox(t *testing.T) {
	c, _ := servidorFake(t, map[string]string{
		"/users/buzon@empresa.com/mailFolders":                    `{"value":[{"id":"INB","displayName":"Bandeja de entrada"}]}`,
		"/users/buzon@empresa.com/mailFolders/Inbox/childFolders": `{"value":[{"id":"PRU2","displayName":"Pruebas"}]}`,
	})

	id, found, err := c.FindInboxFolder(context.Background(), "buzon@empresa.com", "Pruebas")
	if err != nil {
		t.Fatalf("FindInboxFolder devolvió error: %v", err)
	}
	if !found || id != "PRU2" {
		t.Errorf("FindInboxFolder = (%q,%v), want (\"PRU2\",true)", id, found)
	}
}

// TestFindInboxFolderNoExiste: si la carpeta no está en ninguno de los dos
// sitios, se informa found=false y SIN error. main.go se detiene con eso: no debe
// caer en la Bandeja de entrada real.
func TestFindInboxFolderNoExiste(t *testing.T) {
	c, _ := servidorFake(t, map[string]string{
		"/users/buzon@empresa.com/mailFolders":                    `{"value":[{"id":"INB","displayName":"Bandeja de entrada"}]}`,
		"/users/buzon@empresa.com/mailFolders/Inbox/childFolders": sinMensajes,
	})

	id, found, err := c.FindInboxFolder(context.Background(), "buzon@empresa.com", "Prueba")
	if err != nil {
		t.Fatalf("FindInboxFolder devolvió error: %v", err)
	}
	if found || id != "" {
		t.Errorf("FindInboxFolder = (%q,%v), want (\"\",false)", id, found)
	}
}

// TestFindInboxFolderCaseInsensitive: el nombre de config.env no tiene por qué
// coincidir en mayúsculas/minúsculas con el del buzón.
func TestFindInboxFolderCaseInsensitive(t *testing.T) {
	c, _ := servidorFake(t, map[string]string{
		"/users/buzon@empresa.com/mailFolders": `{"value":[{"id":"PRU1","displayName":"Pruebas"}]}`,
	})

	id, found, err := c.FindInboxFolder(context.Background(), "buzon@empresa.com", "PRUEBAS")
	if err != nil {
		t.Fatalf("FindInboxFolder devolvió error: %v", err)
	}
	if !found || id != "PRU1" {
		t.Errorf("FindInboxFolder(%q) = (%q,%v), want (\"PRU1\",true)", "PRUEBAS", id, found)
	}
}

// TestResolveChildFolderCreaBajoElPadre: al crear /Procesados, el POST debe ir a
// las hijas de la carpeta de entrada (no de Inbox).
func TestResolveChildFolderCreaBajoElPadre(t *testing.T) {
	const ruta = "/users/buzon@empresa.com/mailFolders/AAA123/childFolders"
	var creada map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ruta {
			http.Error(w, "ruta no esperada: "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			json.NewDecoder(r.Body).Decode(&creada)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"PROC1","displayName":"Procesados"}`))
			return
		}
		w.Write([]byte(sinMensajes)) // el GET no la encuentra → se crea
	}))
	defer srv.Close()

	c := &Client{http: &http.Client{Transport: redirigir{destino: srv.URL}}}
	id, err := c.ResolveChildFolder(context.Background(), "buzon@empresa.com", "AAA123", "Procesados")
	if err != nil {
		t.Fatalf("ResolveChildFolder devolvió error: %v", err)
	}
	if id != "PROC1" {
		t.Errorf("ResolveChildFolder = %q, want %q", id, "PROC1")
	}
	if creada["displayName"] != "Procesados" {
		t.Errorf("se creó la carpeta %q, want %q", creada["displayName"], "Procesados")
	}
}
