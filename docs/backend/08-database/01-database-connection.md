## 🔌 Database Connection

Odac automatically connects to your MySQL database when you provide the configuration.

### Configuration

Add your database credentials to `config.json`:

```json
{
  "database": {
    "host": "localhost",
    "user": "your_username",
    "password": "your_password",
    "database": "your_database_name"
  }
}
```

### Multiple Databases

You can configure multiple database connections:

```json
{
  "database": {
    "default": {
      "host": "localhost",
      "user": "user1",
      "password": "pass1",
      "database": "main_db"
    },
    "analytics": {
      "host": "analytics.example.com",
      "user": "user2",
      "password": "pass2",
      "database": "analytics_db"
    }
  }
}
```

Access different databases:

```javascript
// Default database
const users = await Odac.Mysql.table('users').get()

// Specific database
const stats = await Odac.Mysql.database('analytics').table('stats').get()
```

### Environment Variables

For security, use environment variables for sensitive data:

**.env file:**
```
DB_HOST=localhost
DB_USER=myuser
DB_PASSWORD=mypassword
DB_NAME=mydatabase
```

**config.json:**
```json
{
  "database": {
    "host": "${DB_HOST}",
    "user": "${DB_USER}",
    "password": "${DB_PASSWORD}",
    "database": "${DB_NAME}"
  }
}
```

### Connection Options

Available configuration options:

- `host` - Database server hostname (default: `localhost`)
- `user` - Database username
- `password` - Database password
- `database` - Database name
- `type` - Database type (currently only `mysql` is supported)

### Automatic Connection

The connection is established automatically when your application starts. You don't need to write any connection code - just use `Odac.Mysql` in your controllers.

```javascript
module.exports = async function (Odac) {
  // Connection is already established
  const users = await Odac.Mysql.table('users').get()
  
  Odac.set('users', users)
  Odac.View.set({ skeleton: 'main', content: 'users' })
}
```
