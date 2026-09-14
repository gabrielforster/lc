# lc

[English](README.md) · **Português (BR)** · [Español](README.es.md)

Um túnel reverso auto-hospedado. Ele torna serviços de uma máquina atrás de NAT
acessíveis em um endereço público, **sem exigir que ninguém instale nada para se
conectar** — um navegador, um cliente TCP comum e um cliente Minecraft original
simplesmente conectam.

```
              ┌──────────────── VPS (lcd) ────────────────┐         NAT
 browser ────▶│ :443/:80  HTTP reverse proxy, by Host     │           │
 mc client ──▶│ :25565    sniffing router, by handshake   │─ yamux ───┼─▶ agent (lc) ─▶ local services
 tcp client ─▶│ :20000+   per-tunnel listener             │  session  │
              │ :7000     control listener                │◀── dials ─┤
              └───────────────────────────────────────────┘           │
```

Nada disca para dentro da rede doméstica. O agente disca **para fora** e mantém
uma sessão aberta; o servidor empurra por ela um stream multiplexado para cada
conexão que chega.

## Tipos de túnel

| Tipo | Roteado por | Observações |
|---|---|---|
| `tcp` | uma porta pública dedicada | Atribuída a partir de uma faixa e então **reservada** — o mesmo túnel recebe a mesma porta de volta após reconexões e reinícios |
| `http` | cabeçalho `Host` | Encaminhado na camada 7, então `X-Forwarded-For`/`-Proto`, keep-alive e upgrades de WebSocket funcionam |
| `minecraft` | hostname no handshake do Java | Uma única porta atende todos os túneis Minecraft; o agente reescreve o handshake para que os jogadores mantenham seus IPs reais |

## Início rápido, tudo local

```sh
go build -o lcd ./cmd/lcd
go build -o lc  ./cmd/lc

# Gere uma credencial e conceda a ela o que pode reivindicar.
./lcd admin token -label laptop            # exibe o segredo uma única vez
./lcd admin grant -token 1 -kind port_auto
./lcd admin grant -token 1 -kind wildcard -value .mc.localhost

./lcd -control 127.0.0.1:7000 -http 127.0.0.1:8080 -minecraft 127.0.0.1:25565
```

`lc.json` na máquina atrás do NAT:

```json
{
  "server": "127.0.0.1:7000",
  "token": "<o segredo exibido acima>",
  "tunnels": [
    {"name": "web",      "kind": "http",      "host": "web.mc.localhost",  "local_addr": "127.0.0.1:9999"},
    {"name": "survival", "kind": "minecraft", "host": "play.mc.localhost", "local_addr": "127.0.0.1:25565"},
    {"name": "ssh",      "kind": "tcp",       "local_addr": "127.0.0.1:22"}
  ]
}
```

```sh
./lc -config lc.json
curl -H 'Host: web.mc.localhost' http://127.0.0.1:8080/
```

## Documentação

> A documentação detalhada está disponível apenas em inglês por enquanto.

| | |
|---|---|
| [Implantação](docs/deployment.md) | Configuração da VPS, systemd, firewall, com e sem domínio |
| [Arquitetura](docs/architecture.md) | Como funciona e por que tem esse formato |
| [Protocolo de controle](docs/protocol.md) | O protocolo entre agente e servidor |
| [Executando o lc](docs/operations.md) | Flags, configuração, permissões, TLS, timeouts de ociosidade |
| [Minecraft](docs/minecraft.md) | Roteamento por handshake, IPs reais dos jogadores **e o aviso sobre `online-mode=false`** |

> [!WARNING]
> Rodar um servidor de Minecraft por trás disso exige `online-mode=false`, o que
> desativa a verificação de sessão da Mojang e faz a whitelist considerar apenas
> o nome de usuário. Um firewall restringindo a porta 25565 ao túnel passa a ser
> a única coisa impedindo que alguém se passe por outro jogador trivialmente.
> Leia
> [a seção de segurança](docs/minecraft.md#security-online-modefalse-and-why-the-firewall-is-load-bearing)
> antes de expor um servidor.

## Estado

O estado do servidor fica em um arquivo SQLite (`-db`, padrão `lc.db`): tokens,
suas permissões, domínios reivindicados e reservas de porta. Os segredos dos
tokens são armazenados com hash e exibidos apenas uma vez, na criação.

## Testes

```sh
go test ./...
go test -race ./...
```

Os testes de ponta a ponta executam um servidor e um agente reais sobre sockets
reais, incluindo um cliente e um servidor Minecraft falsos, então nenhuma JVM é
necessária.

## Situação atual

Funcionando hoje: TCP puro, HTTP e HTTPS com terminação TLS, Minecraft com IPs
reais dos jogadores, reivindicação de domínios personalizados, reservas de porta
duráveis, reconexão com backoff, limites de conexão por túnel e recuperação de
conexões ociosas.

**A interface web do `lc` ainda não foi construída — está planejada.** O
registro usa SQLite em vez de um arquivo de configuração justamente para que
construí-la seja escrever um frontend sobre `internal/store`, e não uma
migração: os tokens já são armazenados com hash em vez de texto puro, e as
falhas de reivindicação já carregam motivos tipados que uma interface pode
exibir. Até lá, `lcd admin` e `lc domains` cobrem o mesmo terreno pela linha de
comando.

Fora do planejamento: Bedrock (é UDP), múltiplos servidores ou alta
disponibilidade. O túnel autenticar com a Mojang por conta própria não está
implementado, mas é a correção certa caso os túneis de Minecraft venham a ser
expostos a desconhecidos — veja [a documentação do Minecraft](docs/minecraft.md).
