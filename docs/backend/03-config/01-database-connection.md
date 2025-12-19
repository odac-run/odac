## 🔌 Database Connection

When you add a `mysql` object to your `config.json`, the system will automatically connect to your MySQL database. No separate connection setup is needed in your code.

### Basic Configuration

```json
{
  "mysql": {
    "host": "localhost",
    "user": "your_user",
    "password": "your_password",
    "database": "your_database"
  }
}
```

Once this is configured, you can directly use `Odac.Mysql` commands to run queries.

### Using Environment Variables

For better security, especially in production, you can use environment variables for sensitive information:

**config.json:**
```json
{
  "mysql": {
    "host": "${MYSQL_HOST}",
    "user": "${MYSQL_USER}",
    "password": "${MYSQL_PASSWORD}",
    "database": "myapp"
  }
}
```

**.env:**
```bash
MYSQL_HOST=localhost
MYSQL_USER=root
MYSQL_PASSWORD=super_secret_123
```

The `.env` file should be added to `.gitignore` to keep your credentials secure.

### Mixed Approach

You can also mix direct values with environment variables:

```json
{
  "mysql": {
    "host": "localhost",
    "user": "root",
    "password": "${MYSQL_PASSWORD}",
    "database": "myapp"
  }
}
```

This way, non-sensitive values are directly in the config while passwords remain in the `.env` file.
