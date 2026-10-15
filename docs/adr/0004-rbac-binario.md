# ADR 0004: RBAC binário (usuário autenticado ou admin), sem permissão por recurso

## Status

Aceito

## Contexto

As rotas precisam distinguir quem lê de quem altera dados de cliente. O domínio não tem noção de dono nem de
tenant: é uma API interna de gestão de pedidos, não um autoatendimento em que o chamador só enxerga os próprios registros.

## Decisão

Dois papéis, lidos da claim `roles` do JWT (`internal/auth`): qualquer chamador autenticado (`user`, o padrão
quando a claim não existe) acessa toda leitura e opera pedidos; `admin` é exigido para criar, alterar e remover
clientes, pelo middleware `requireRole(auth.RoleAdmin)` (`internal/httpapi/middleware.go`). Toda rota é registrada
por `protected(...)` em `handler.go`, então nenhuma fica pública por omissão.

## Alternativas consideradas

- **Autorização por dono do recurso**: não corresponde ao domínio, o cliente cadastrado não é o chamador da API.
- **Papel único**: mais simples, mas elimina a fronteira leitura x escrita.
- **Permissões finas (`orders:write`, `customers:delete`)**: proporcional a uma API com vários times e clientes;
  aqui há dois tipos de chamador, dois papéis bastam.

## Consequências

- Positiva: a regra fica em um middleware reutilizado, fácil de auditar com `grep requireRole`.
- Aceita: um terceiro tipo de chamador (um auditor somente leitura, por exemplo) obriga a tocar rota por rota;
  não há hierarquia de papéis.
