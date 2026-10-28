# Observabilité d'une API Go : slog, runtime/metrics, Prometheus et OpenTelemetry

[English version](README.md)

Une petite API de factures en `net/http` standard, instrumentée pour que les
trois signaux renvoient les uns aux autres :

- **logs** (`log/slog`, JSON) : chaque ligne de log d'une requête porte `trace_id` et `span_id` ;
- **métriques** (Prometheus, méthode RED + runtime Go) : les mesures de durée portent
  le `trace_id` en **exemplar**, si bien qu'un point du graphe de latence dans Grafana ouvre la trace ;
- **traces** (OpenTelemetry) : span serveur par `otelhttp`, span enfant dans le
  dépôt, propagation W3C `traceparent`, échantillonnage en tête dans l'app et
  **tail sampling** dans le Collector (erreurs et requêtes lentes gardées, ~10 % du reste).

Code d'accompagnement de l'article « Observabilité d'une API Go : slog, runtime/metrics,
Prometheus et OpenTelemetry », par Odilon Hugonnot ([web-developpeur.com](https://web-developpeur.com)).

C'est un exemple pédagogique, pas un modèle de production.

## Organisation

```
main.go          serveurs (public :8080, métriques internes :9091), arrêt propre sur SIGTERM
server.go        routes, handlers, ordre des middlewares
store.go         dépôt en mémoire, span enfant, Invoice.LogValue (pas de données perso dans les logs)
logging.go       handler slog qui ajoute trace_id/span_id, filet de sécurité de masquage
metrics.go       métriques RED étiquetées par motif de route, exemplars, collecteur runtime Go
tracing.go       TracerProvider, ParentBased(TraceIDRatioBased), exporteur OTLP
demo.go          ?delay= et ?fail= (seulement avec DEMO_FAULTS=1)
cmd/loadgen      générateur de trafic : requêtes normales, lentes et en erreur
*_test.go        httptest, tracetest, prometheus/testutil, corrélation logs/traces
docker-compose.yml, otel-collector/, prometheus/, grafana/   le labo local
scripts/e2e.sh   test de bout en bout de toute la pile
```

## Lancer

```sh
make run          # DEMO_FAULTS=1 go run .   (API sur :8080, métriques sur 127.0.0.1:9091)
curl localhost:8080/invoices/1
curl -X POST localhost:8080/invoices -d '{"customer":"Bob","email":"bob@example.com","amount_cents":500,"currency":"EUR"}'
curl 127.0.0.1:9091/metrics
```

Sans `OTEL_EXPORTER_OTLP_ENDPOINT`, les spans sont créés (les logs ont toujours
un `trace_id`) mais pas exportés.

```sh
make test         # go test ./...
make lint         # go vet + gofmt -l
make rules        # promtool check rules / test rules / check config (Docker)
```

### La pile complète

```sh
cp .env.example .env    # puis renseigner GRAFANA_ADMIN_PASSWORD
make up                 # docker compose up -d --build
make demo               # 1000 requêtes : 5 % d'erreurs, 5 % de lentes, le reste normal
make down               # docker compose down -v
make e2e                # up, démo, vérifications par les API HTTP, down -v
```

| URL                         | Quoi                                        |
|-----------------------------|---------------------------------------------|
| http://127.0.0.1:8080       | l'API                                       |
| http://127.0.0.1:3000       | Grafana (admin / mot de passe du .env), tableau « Invoice API - RED » |
| http://127.0.0.1:16686      | interface Jaeger                            |
| http://127.0.0.1:9090       | Prometheus (alertes sous /alerts)           |

## Ce que montre chaque brique

**Labels par motif de route.** Les métriques sont étiquetées avec `r.Pattern`
(`/invoices/{id}`), jamais `r.URL.Path` (`/invoices/42`) : une série par route,
pas une par facture. Une requête sans route reçoit `route="unmatched"`, une
méthode inconnue `method="OTHER"` : un client ne peut pas créer de séries à
volonté. `TestRequestCounterUsesRoutePattern` échoue si l'on remet `r.URL.Path`.

**Ordre des middlewares.** `otelhttp -> instrument -> mux`. Le mux écrit
`r.Pattern` dans le `*http.Request` qu'il reçoit ; `instrument` doit lui passer
ce même pointeur (pas de `r.WithContext`) pour le relire ensuite. Copier la
requête casse d'un coup les labels, les noms de spans et les exemplars ;
plusieurs tests le détectent.

**Port de métriques séparé.** `/metrics` écoute sur `:9091`, que le compose ne
publie pas. Ce n'est pas l'API, cela révèle l'interne (version de Go, mémoire,
routes), et le scrape ne doit pas dépendre de l'authentification, du rate
limiting ou du load balancer du port public.

**Logs.** `traceHandler` enveloppe le handler JSON et ajoute `trace_id`/`span_id`
depuis le contexte (utiliser `InfoContext` et consorts). Les données
personnelles restent dehors : `Invoice.LogValue` ne logge que l'id, le montant
et la devise, le log d'un corps invalide ne recopie pas le corps, et une liste
`ReplaceAttr` (`email`, `authorization`…) masque les erreurs d'inattention.

**Métriques runtime.** `collectors.NewGoCollector` avec
`WithGoCollectorRuntimeMetrics(MetricsGC, MetricsMemory, MetricsScheduler)`
expose des séries de `runtime/metrics` (GC, tas, latences de l'ordonnanceur).

**Échantillonnage.** L'app échantillonne en tête avec
`ParentBased(TraceIDRatioBased(TRACE_SAMPLE_RATIO))` (1 par défaut) : une
requête arrivée avec un `traceparent` échantillonné le reste. Dans le compose le
ratio vaut 1 et le Collector décide en queue, une fois la trace complète
(`decision_wait: 5s` pour la démo) : `status_code: ERROR` et `latency > 500ms`
sont toujours gardées, `probabilistic: 10` garde une part du reste.

**Exemplars.** L'histogramme de durée est alimenté par `ObserveWithExemplar`
avec le `trace_id` ; `/metrics` sert l'OpenMetrics (nécessaire aux exemplars) et
Prometheus tourne avec `--enable-feature=exemplar-storage`. La datasource
Prometheus de Grafana relie `trace_id` à la datasource Jaeger.

**Alertes sur le budget d'erreur.** `prometheus/rules/slo.yml` : SLO de 99,9 %
sur 30 jours, burn rate multi-fenêtres. Page à 14,4× (2 % du budget en 1 h,
vérifié sur 1 h et 5 min), ticket à 6× (5 % en 6 h, sur 6 h et 30 min).
`slo_test.yml` est un test unitaire `promtool test rules`.

### Les logs dans Loki (non inclus)

La pile n'a pas de stockage de logs : ils partent sur stdout. Pour passer d'une
ligne de log à sa trace, envoyer les lignes JSON à Loki (Grafana Alloy, ou le
receiver `filelog` du Collector qui lit les logs des conteneurs) et ajouter un
champ dérivé à la datasource Loki :

```yaml
jsonData:
  derivedFields:
    - name: trace_id
      matcherRegex: '"trace_id":"(\w+)"'
      datasourceUid: jaeger
      url: "$${__value.raw}"
```

Laissé de côté pour garder un labo petit : il faut accéder aux fichiers de logs
de Docker ou à un plugin de logging driver, donc plus de privilèges que le reste.

## Choix

- **Images séparées plutôt que `grafana/otel-lgtm`.** Cette image est parfaite
  pour un premier coup d'œil, mais le tail sampling exige le Collector *contrib*
  (`tail_sampling` n'est pas dans la distribution core), et voir la configuration
  de chaque composant est l'objet de l'article.
- **Jaeger 2.20.0, pas 2.22.** Jaeger 2.21 a retiré les endpoints HTTP v1
  (`/api/services`, `/api/traces`) qu'appelle la datasource Jaeger de Grafana 13.2 :
  avec la 2.22.0, son test de santé renvoyait `404 Not Found`. Passer à Tempo
  est la vraie solution à terme.
- **Durcissement, là où beaucoup de tutoriels font autrement :** aucun conteneur
  en root, `cap_drop: ALL` et `no-new-privileges`, mot de passe Grafana dans
  `.env` (jamais `admin/admin` dans le compose), interfaces d'administration sur
  `127.0.0.1`, Prometheus sans `--web.enable-lifecycle`, image de l'app
  distroless `nonroot`.

## Versions

Go 1.26 (toolchain go1.26.10), go.opentelemetry.io/otel v1.47.0, otelhttp v0.72.0,
prometheus/client_golang v1.25.0. Images : otel/opentelemetry-collector-contrib:0.164.0,
jaegertracing/jaeger:2.20.0, prom/prometheus:v3.15.1, grafana/grafana:13.2.5,
golang:1.26.10-alpine3.24, gcr.io/distroless/static-debian13:nonroot (épinglée par digest).

## Limites

- Exemple pédagogique : dépôt en mémoire, pas d'authentification, pas de TLS
  entre composants, Jaeger en mémoire, intervalles de démo (scrape 5 s,
  `decision_wait` 5 s).
- `DEMO_FAULTS=1` ne doit jamais être activé sur un vrai service.
- Un exemplar ou un `trace_id` de log peut viser une trace que le Collector a
  abandonnée (environ 90 % du trafic normal). Les erreurs et les requêtes lentes,
  celles sur lesquelles on clique, sont gardées.
- Le tail sampling exige que tous les spans d'une trace passent par le même
  Collector : avec plusieurs réplicas, placer un exporteur `loadbalancing` devant.

## Licence

MIT, Copyright (c) 2026 Odilon Hugonnot.
