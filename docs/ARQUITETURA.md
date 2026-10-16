# Arquitetura técnica: go-order-api

Explicação técnica de como a API de pedidos em Go é montada. O contrato vem do `fastapi-order-api`; aqui está o que é próprio desta implementação. Decisões formais em `docs/adr/` (0001 a 0003).

## 1. Visão geral

Dois executáveis (`cmd/api`, `cmd/worker`) sobre pacotes em `internal/`. Go 1.27, `net/http` puro (ServeMux, sem framework web), `pgx/v5`, `amqp091-go`, `golang-jwt/v5`, `shopspring/decimal`, `google/uuid`, `x/time/rate`. Imagem: binário estático (`CGO_ENABLED=0`) em `gcr.io/distroless/static-debian12:nonroot`.

## 2. Pacotes

```
internal/domain     Customer, Order, Item, eventos. Sem I/O.
internal/app        serviços (CustomerService, OrderService), busca e ports.go
internal/apperr     erro único com código do catálogo (VALIDATION-xx, CONFLICT-xx...)
internal/auth       verificação RS256
internal/config     variáveis de ambiente e regras de produção
internal/httpapi    handlers, DTOs, middlewares, rate limit, envelope de erro
internal/postgres   repositórios pgx, busca, migrations embutidas
internal/messaging  publisher/consumer AMQP, processor, e-mail SMTP
```

A convenção idiomática de Go aparece em `app/ports.go`: as interfaces (`CustomerRepository`, `OrderRepository`, `Repositories`, `Store` com `Repos()` e `InTx`, `EventPublisher`) ficam **no pacote que as consome**, não no que as implementa. Os serviços testam contra `memstore_test.go`, um repositório em memória, sem banco.

## 3. Fluxo de uma requisição

A cadeia é montada à mão com `chain()`: `requestID`, `securityHeaders`, `bodyLimit` (`http.MaxBytesReader`, conta também corpo chunked), `recoverer` (panic vira `INTERNAL-00`), `ratelimit`. Depois, autenticação Bearer RS256 e checagem de papel por rota. O handler decodifica o DTO, chama o serviço e devolve JSON; erros passam por `errors.go`, que converte `apperr` em envelope `{message, code, params, request_id}`.

Servidor com timeouts explícitos: ReadHeader 5 s, Read 15 s, Write 30 s, Idle 60 s, `MaxHeaderBytes` 16 KB. Desligamento gracioso com contexto e `SHUTDOWN_TIMEOUT`. Logs `slog` em JSON.

## 4. Persistência

- `postgres.Store` guarda o pool. `InTx` abre uma transação e entrega `Repositories` ligados a ela. Os repositórios aceitam uma interface mínima `db` que pool e transação satisfazem, então o mesmo código roda dentro e fora de transação.
- `translate()` converte SQLSTATE e nome de constraint em códigos de negócio.
- Migrations embutidas (`migrate.go`), aplicadas na partida.
- **Busca** (`search.go`): cada entidade declara um mapa de campos com tipo (`typeString`, `typeUUID`, `typeInt`, `typeTime`, `typeEnum`). Valores são coagidos ao tipo da coluna antes do SQL, então tipo errado é `VALIDATION-05` e não erro do driver. O texto SQL só contém nomes do mapa; valores são sempre parâmetros.
- Dinheiro em `decimal.Decimal`, com `MaxUnitPrice` de 99999999.99.
- Concorrência: `version` otimista, `FOR SHARE`/`FOR UPDATE` na criação de pedido e exclusão de cliente, índices únicos parciais com `deleted_at IS NULL`.

## 5. Mensageria

`amqp091-go` com publisher confirms; `processor.go` decide ack, retry ou DLQ; `email.go`/`smtp.go` formatam e enviam. Em produção exigem `amqps` e STARTTLS. Sem outbox.

## 6. Segurança

Falha rápida na partida com configuração de produção insegura; só chave pública no verificador; `aud`/`iss` obrigatórios em produção; resposta de cliente sem PII; cabeçalhos de segurança em todas as respostas; rate limit por usuário e IP (`x/time/rate`, por processo).

## 7. Testes

118 funções de teste em 17 arquivos. Unitários com repositório em memória; integração com PostgreSQL (`TEST_DATABASE_URL`) e RabbitMQ reais; SMTP falso. Sempre com `-race`. Piso de cobertura 80% (`scripts/check-coverage.sh`).

## 8. CI e entrega

`golangci-lint`, `go test -race` com cobertura, `govulncheck`, build das imagens, dependabot com auto merge. A imagem distroless nonroot tem ~13 MB.

## 9. Trade-offs

| Escolha | Ganho | Custo |
|---|---|---|
| `net/http` sem framework | zero dependência de web, tudo explícito | middleware e roteamento escritos à mão |
| Interfaces no consumidor | testes simples, sem mocks gerados | cuidado para não duplicar contratos |
| `db` mínima para pool e tx | repositório único | pouca checagem em tempo de compilação do SQL |
| Mapa de campos da busca à mão | SQL previsível | lista precisa acompanhar o schema |
| Sem outbox | simples | evento pode se perder em crash |
