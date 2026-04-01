# Todo API

Une API REST de gestion de tâches (todos) construite avec Go et Gin, avec support des alertes en temps réel via Server-Sent Events.

Le dossier `vendor` est présent pour éviter de re-télécharger les librairies à chaque déploiement sur Clever Cloud.

## Auteur

Louis Michault DO3

## Prérequis

- [Docker](https://www.docker.com/) et Docker Compose
- Un fichier `.env` correctement configuré (voir ci-dessous)

## Configuration

Crée un fichier `.env` à la racine du projet :

```env
PORT=3000
POSTGRES_USER=postgres
POSTGRES_PASSWORD=postgres
POSTGRES_DB=todos
POSTGRESQL_ADDON_URI=postgresql://postgres:postgres@db:5432/todos
APP_NAME=MyAwesomeApp
```

> L'hôte dans `POSTGRESQL_ADDON_URI` doit être `db` (nom du service Docker), pas `localhost`.

## Lancer l'application

```bash
docker compose up --build
```

L'API est accessible sur `http://localhost:3000`.

## Endpoints

### Santé

| Méthode | Route | Description |
|---------|-------|-------------|
| GET | `/health` | État de l'application et de la base de données |

**Exemple de réponse :**
```json
{
  "status": "ok",
  "app": "MyAwesomeApp",
  "database": "connected"
}
```

---

### Todos

| Méthode | Route | Description |
|---------|-------|-------------|
| GET | `/todos` | Lister tous les todos |
| GET | `/todos?status=pending` | Filtrer par statut (`pending` ou `done`) |
| GET | `/todos/overdue` | Lister les todos en retard |
| POST | `/todos` | Créer un todo |
| PATCH | `/todos/:id` | Modifier un todo |
| DELETE | `/todos/:id` | Supprimer un todo |

**Corps de requête pour créer ou modifier un todo :**
```json
{
  "title": "Mon todo",
  "description": "Description optionnelle",
  "due_date": "2025-12-31T00:00:00Z",
  "status": "pending"
}
```

> `title` est obligatoire. Les autres champs sont optionnels.

**Statuts disponibles :** `pending`, `done`

---

### Alertes (Server-Sent Events)

| Méthode | Route | Description |
|---------|-------|-------------|
| GET | `/alerts` | Se connecter au flux d'alertes SSE |
| POST | `/todos/:id/notify` | Envoyer une alerte pour un todo |

**Se connecter au flux d'alertes :**
```bash
curl -N http://localhost:3000/alerts
```

**Envoyer une alerte :**
```bash
curl -X POST http://localhost:3000/todos/1/notify
```

Un ping est envoyé toutes les 30 secondes pour maintenir la connexion active.

---

## Exemples

**Créer un todo :**
```bash
curl -X POST http://localhost:3000/todos \
  -H "Content-Type: application/json" \
  -d '{"title": "Acheter du pain", "due_date": "2025-06-01T00:00:00Z"}'
```

**Lister les todos en attente :**
```bash
curl http://localhost:3000/todos?status=pending
```

**Marquer un todo comme terminé :**
```bash
curl -X PATCH http://localhost:3000/todos/1 \
  -H "Content-Type: application/json" \
  -d '{"status": "done"}'
```

**Supprimer un todo :**
```bash
curl -X DELETE http://localhost:3000/todos/1
```

## Déploiement sur Clever Cloud

L'application est compatible avec Clever Cloud (runtime Go). Elle écoute sur `0.0.0.0:8080` comme requis par la plateforme.

Les variables d'environnement suivantes doivent être configurées dans la console Clever Cloud :

- `POSTGRESQL_ADDON_URI` — injectée automatiquement si un addon PostgreSQL est lié
- `APP_NAME` — nom affiché dans `/health`