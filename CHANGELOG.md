# Changelog

Formato baseado em [Keep a Changelog](https://keepachangelog.com/pt-BR/1.0.0/). O projeto não versiona
releases (`main` é a linha de entrega); as entradas agrupam o histórico por tema, não por tag.

## [Unreleased]

### Added

- API de pedidos e clientes em Go portada da especificação do `fastapi-order-api`: domínio, casos de uso,
  repositórios PostgreSQL (`pgx`), busca com filtros tipados, JWT RS256, worker de e-mail via RabbitMQ.
- Testes de integração: repositórios e HTTP ponta a ponta contra PostgreSQL real, publisher e consumer contra
  RabbitMQ real (retry, DLQ).
- Piso de cobertura no CI (80% de `./internal/...`).

### Security

- Actions fixadas por SHA, Dependabot semanal, auto-merge para patch e minor, `govulncheck` fixado por versão.
- `golang.org/x/text` atualizado por GO-2026-6629 e GO-2026-5970.
- A resposta de cliente expõe só `id` e `name`; documento, passaporte e e-mail ficam fora da API e dos logs.

### Changed

- Itens de um pedido passam a ser atualizados em um único comando SQL.
- Retry e DLQ só reconhecem (`ack`) a mensagem original depois da confirmação do broker para a cópia.
- O anúncio da mudança de status sobrevive ao cancelamento da requisição e registra falha ao buscar o cliente.
