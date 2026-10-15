# Domínio: gestão de pedidos

A especificação funcional (entidades, invariantes, eventos e catálogo de erros) é a do
[`fastapi-order-api`](https://github.com/ronybrand/fastapi-order-api/blob/main/DOMAIN.md); este projeto a implementa
em Go e não a duplica, para que haja uma única fonte. Este arquivo mapeia a especificação para o código e registra
onde esta implementação acrescenta ou escolhe diferente.

## Onde cada regra vive

| Regra da especificação | Código |
|---|---|
| Validação de cliente (nome, documento canônico, passaporte, e-mail) | `internal/domain/customer.go` |
| Itens, total derivado, máquina de estados OPEN, CONFIRMED, CANCELED | `internal/domain/order.go` |
| Evento `OrderStatusChanged` (contrato da fila) | `internal/domain/events.go` |
| Criar pedido exige cliente existente; excluir cliente exige zero pedidos ativos | `internal/app/order_service.go`, `customer_service.go` |
| Unicidade de documento e passaporte entre ativos | índices parciais em `internal/postgres/migrations/0001_init.up.sql` |
| Lock otimista (`version`) | `internal/postgres/customers.go`, `orders.go` |
| Busca, filtros e limites | `internal/app/search.go`, `internal/postgres/search.go` |
| Códigos de erro | `internal/apperr/codes.go` |
| Retry, DLQ e deduplicação das notificações | `internal/messaging/processor.go` |

## Diferenças em relação à especificação

- **Resposta de cliente**: só `id` e `name`. Documento, passaporte e e-mail são PII e não voltam pela API.
- **Códigos acrescentados**: `RESOURCE-NOT-FOUND-00` (rota inexistente) e `VALIDATION-13` (método não permitido), para que
  404 e 405 do roteador saiam no mesmo envelope `{message, code, params, request_id}`.
- **Campos pesquisáveis**: `customers` aceita `id, name, tax_id, passport_number, email, version, created_at, updated_at,
  created_by, updated_by`; `orders` aceita `id, customer_id, status, version, created_at, updated_at, created_by,
  updated_by`. A lista está em `internal/postgres/search.go`. Colunas de exclusão lógica não são pesquisáveis nem ordenáveis.
- **Total do pedido** aparece como número JSON com duas casas (`10.30`), nunca como float.
- **Notificação**: publicada depois do commit, sem outbox (ADR 0002).
