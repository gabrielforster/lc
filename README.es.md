# lc

[English](README.md) · [Português (BR)](README.pt-BR.md) · **Español**

Un túnel inverso autoalojado. Hace que los servicios de una máquina detrás de
NAT sean accesibles en una dirección pública, **sin pedirle a nadie que instale
nada para conectarse**: un navegador, un cliente TCP común y un cliente de
Minecraft original simplemente conectan.

```
              ┌──────────────── VPS (lcd) ────────────────┐         NAT
 browser ────▶│ :443/:80  HTTP reverse proxy, by Host     │           │
 mc client ──▶│ :25565    sniffing router, by handshake   │─ yamux ───┼─▶ agent (lc) ─▶ local services
 tcp client ─▶│ :20000+   per-tunnel listener             │  session  │
              │ :7000     control listener                │◀── dials ─┤
              └───────────────────────────────────────────┘           │
```

Nada marca hacia dentro de la red doméstica. El agente marca **hacia fuera** y
mantiene una sesión abierta; el servidor envía por ella un stream multiplexado
por cada conexión entrante.

## Tipos de túnel

| Tipo | Enrutado por | Notas |
|---|---|---|
| `tcp` | un puerto público dedicado | Asignado desde un rango y luego **reservado**: el mismo túnel recupera el mismo puerto tras reconexiones y reinicios |
| `http` | cabecera `Host` | Redirigido en capa 7, así que `X-Forwarded-For`/`-Proto`, keep-alive y las actualizaciones a WebSocket funcionan |
| `minecraft` | hostname en el handshake de Java | Un solo puerto atiende todos los túneles de Minecraft; el agente reescribe el handshake para que los jugadores conserven sus IP reales |

## Inicio rápido, todo local

```sh
go build -o lcd ./cmd/lcd
go build -o lc  ./cmd/lc

# Genera una credencial y concédele lo que puede reclamar.
./lcd admin token --label laptop            # muestra el secreto una sola vez
./lcd admin grant --token 1 --kind port_auto
./lcd admin grant --token 1 --kind wildcard --value .mc.localhost

./lcd --control 127.0.0.1:7000 --http 127.0.0.1:8080 --minecraft 127.0.0.1:25565
```

`lc.json` en la máquina detrás del NAT:

```json
{
  "server": "127.0.0.1:7000",
  "token": "<el secreto mostrado arriba>",
  "tunnels": [
    {"name": "web",      "kind": "http",      "host": "web.mc.localhost",  "local_addr": "127.0.0.1:9999"},
    {"name": "survival", "kind": "minecraft", "host": "play.mc.localhost", "local_addr": "127.0.0.1:25565"},
    {"name": "ssh",      "kind": "tcp",       "local_addr": "127.0.0.1:22"}
  ]
}
```

```sh
./lc --config lc.json
curl -H 'Host: web.mc.localhost' http://127.0.0.1:8080/
```

Ambos los binarios son árboles de comandos de cobra — `lcd --help`,
`lcd admin grant --help`, `lc domains --help` — e incluyen autocompletado para
la shell mediante `lcd completion zsh`.

> [!IMPORTANT]
> Las flags llevan **dos** guiones: `--control :7000`, no `-control :7000`. Si
> tienes algún script anterior a la migración a cobra, ese es el único cambio que
> necesita.

## Documentación

> Por ahora la documentación detallada solo está disponible en inglés.

| | |
|---|---|
| [Despliegue](docs/deployment.md) | Configuración de la VPS, systemd, firewall, con y sin dominio |
| [Arquitectura](docs/architecture.md) | Cómo funciona y por qué tiene esta forma |
| [Protocolo de control](docs/protocol.md) | El protocolo entre agente y servidor |
| [Ejecutar lc](docs/operations.md) | La línea de comandos, flags, configuración, permisos, TLS, tiempos de inactividad |
| [Minecraft](docs/minecraft.md) | Enrutado por handshake, IP reales de los jugadores **y la advertencia sobre `online-mode=false`** |

> [!WARNING]
> Ejecutar un servidor de Minecraft detrás de esto requiere `online-mode=false`,
> lo que desactiva la verificación de sesión de Mojang y hace que la whitelist
> compare únicamente el nombre de usuario. Un firewall que restrinja el puerto
> 25565 al túnel pasa a ser lo único que impide suplantar a otro jugador de
> forma trivial. Lee
> [la sección de seguridad](docs/minecraft.md#security-online-modefalse-and-why-the-firewall-is-load-bearing)
> antes de exponer un servidor.

## Estado persistente

El estado del servidor vive en un archivo SQLite (`--db`, por defecto `lc.db`):
tokens, sus permisos, dominios reclamados y reservas de puertos. Los secretos de
los tokens se guardan con hash y se muestran una sola vez, al crearlos.

## Pruebas

```sh
go test ./...
go test -race ./...
```

Las pruebas de extremo a extremo ejecutan un servidor y un agente reales sobre
sockets reales, incluidos un cliente y un servidor de Minecraft falsos, así que
no hace falta ninguna JVM.

## Situación actual

Funcionando hoy: TCP puro, HTTP y HTTPS con terminación TLS, Minecraft con IP
reales de los jugadores, reclamación de dominios personalizados, reservas de
puerto duraderas, reconexión con backoff, límites de conexiones por túnel y
recuperación de conexiones inactivas.

**La interfaz web de `lc` todavía no está construida; está planificada.** El
registro usa SQLite en lugar de un archivo de configuración precisamente para
que construirla sea escribir un frontend sobre `internal/store` y no una
migración: los tokens ya se guardan con hash en lugar de texto plano, y los
fallos de reclamación ya llevan motivos tipados que una interfaz puede mostrar.
Mientras tanto, `lcd admin` y `lc domains` cubren lo mismo desde la línea de
comandos.

Fuera de los planes: Bedrock (usa UDP), múltiples servidores o alta
disponibilidad. Que el túnel se autentique con Mojang por su cuenta no está
implementado, pero es la solución correcta si alguna vez se exponen los túneles
de Minecraft a desconocidos: consulta
[la documentación de Minecraft](docs/minecraft.md).
