# ADR 0001: Layout do projeto e direção das dependências

## Contexto

O projeto implementa em Go a especificação do `fastapi-order-api` (clientes, pedidos, itens, notificação por e-mail). A especificação não impõe estrutura; o objetivo é seguir as convenções da linguagem.

## Decisão

- `cmd/api` e `cmd/worker` são apenas composição: leem a configuração, montam as dependências e iniciam o processo. Não há regra de negócio nelas.
- `internal/` impede que outro módulo importe os pacotes.
- Dependências apontam para dentro: `httpapi`, `postgres` e `messaging` conhecem `app` e `domain`; `app` conhece `domain`; `domain` não conhece ninguém além de `apperr`.
- **Interfaces são declaradas por quem as consome** (`app.CustomerRepository`, `httpapi.OrderService`, `messaging.Mailer`), pequenas, e as structs concretas são devolvidas pelos construtores ("accept interfaces, return structs"). Isso permite testar `app` com um store em memória e `httpapi` com fakes, sem banco nem broker.
- Injeção por construtor; opções facultativas por *functional options* (`app.WithClock`, `app.WithLogger`).
- `context.Context` é sempre o primeiro parâmetro. O usuário autenticado viaja no contexto (`app.WithActor`) e é a única fonte das colunas de auditoria.
- Erros são valores: `apperr.Error` carrega `Kind` (status HTTP) e `Code` (catálogo da especificação). Qualquer outro erro vira `INTERNAL-00` sem vazar a mensagem.
- Roteamento com `net/http` (`ServeMux` com método e path, Go 1.22+), logs com `log/slog`, driver `pgx/v5` sem ORM: o SQL é explícito e as regras de bloqueio (`FOR UPDATE`/`FOR SHARE`) ficam visíveis.

## Consequências

Pouca magia e poucas dependências. O custo é escrever SQL e mapeamento à mão, aceito porque o domínio é pequeno e as garantias de concorrência dependem de SQL preciso.
