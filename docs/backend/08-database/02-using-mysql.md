## 🗄️ Using MySQL

Odac provides a powerful query builder for safe and easy database operations.

### Selecting Data

#### Get All Records

```javascript
const users = await Odac.Mysql.table('users').get()
```

#### Get First Record

```javascript
const user = await Odac.Mysql.table('users').where('id', 1).first()
```

#### Select Specific Columns

```javascript
const users = await Odac.Mysql.table('users')
  .select('id', 'name', 'email')
  .get()
```

### Where Conditions

```javascript
// Single condition
const users = await Odac.Mysql.table('users')
  .where('status', 'active')
  .get()

// Multiple conditions (AND)
const users = await Odac.Mysql.table('users')
  .where('status', 'active')
  .where('verified', 1)
  .get()

// OR condition
const users = await Odac.Mysql.table('users')
  .where('role', 'admin')
  .orWhere('role', 'moderator')
  .get()

// Comparison operators
const products = await Odac.Mysql.table('products')
  .where('price', '>', 100)
  .where('stock', '<=', 10)
  .get()

// LIKE
const users = await Odac.Mysql.table('users')
  .where('name', 'LIKE', '%John%')
  .get()

// IN
const users = await Odac.Mysql.table('users')
  .where('role', 'IN', ['admin', 'moderator'])
  .get()
```

### Complex Where Conditions

For complex queries with nested AND/OR conditions, use arrays:

```javascript
// SQL: WHERE (status = 'active' AND verified = 1) OR (role = 'admin')
const users = await Odac.Mysql.table('users')
  .where([
    ['status', 'active'],
    ['verified', 1]
  ])
  .orWhere(['role', 'admin'])
  .get()

// SQL: WHERE status = 'active' AND (role = 'admin' OR role = 'moderator')
const users = await Odac.Mysql.table('users')
  .where('status', 'active')
  .where([
    ['role', 'admin'],
    'OR',
    ['role', 'moderator']
  ])
  .get()

// Complex nested conditions
// SQL: WHERE (status = 'active' AND (role = 'admin' OR role = 'moderator')) 
//      AND (verified = 1 OR email_verified = 1)
const users = await Odac.Mysql.table('users')
  .where([
    ['status', 'active'],
    [
      ['role', 'admin'],
      'OR',
      ['role', 'moderator']
    ]
  ])
  .where([
    ['verified', 1],
    'OR',
    ['email_verified', 1]
  ])
  .get()
```

### Ordering and Limiting

```javascript
// Order by
const users = await Odac.Mysql.table('users')
  .order('created_at', 'desc')
  .get()

// Limit
const users = await Odac.Mysql.table('users')
  .limit(10)
  .get()

// Pagination
const page = 2
const perPage = 20
const users = await Odac.Mysql.table('users')
  .limit((page - 1) * perPage, perPage)
  .get()
```

### Counting Records

```javascript
const userCount = await Odac.Mysql.table('users')
  .where('status', 'active')
  .rows()
```

### Joins

```javascript
const orders = await Odac.Mysql.table('orders')
  .leftJoin('users', 'orders.user_id', 'users.id')
  .select('orders.*', 'users.name', 'users.email')
  .get()
```

### Inserting Data

```javascript
// Insert single record
const result = await Odac.Mysql.table('users').insert({
  name: 'John Doe',
  email: 'john@example.com',
  status: 'active'
})

console.log(result.id) // Inserted ID
console.log(result.affected) // Affected rows
```

### Updating Data

```javascript
const result = await Odac.Mysql.table('users')
  .where('id', 1)
  .set({
    name: 'Jane Doe',
    email: 'jane@example.com'
  })

console.log(result.affected) // Number of updated rows
```

### Deleting Data

```javascript
const result = await Odac.Mysql.table('users')
  .where('id', 1)
  .delete()

console.log(result.affected) // Number of deleted rows
```

### Raw Queries

For complex queries, use raw SQL:

```javascript
// Raw value in select
const users = await Odac.Mysql.table('users')
  .select('id', 'name', Odac.Mysql.raw('COUNT(*) as total'))
  .get()

// Raw query with parameters (SAFE - recommended)
const result = await Odac.Mysql.run('SELECT * FROM users WHERE status = ?', ['active'])
```

#### ⚠️ Security Warning: Mysql.raw()

**CRITICAL**: `Mysql.raw()` bypasses ALL SQL injection protection. Only use with hardcoded, trusted values.

```javascript
// ❌ DANGEROUS - Never do this!
const userInput = await Odac.request('search')
const users = await Odac.Mysql.table('users')
  .where('name', Odac.Mysql.raw(userInput))  // SQL INJECTION RISK!
  .get()

// ✅ SAFE - Use query builder instead
const userInput = await Odac.request('search')
const users = await Odac.Mysql.table('users')
  .where('name', 'LIKE', `%${userInput}%`)  // Automatically escaped
  .get()

// ✅ SAFE - Use with hardcoded values only
const users = await Odac.Mysql.table('users')
  .select('id', 'name', Odac.Mysql.raw('COUNT(*) as total'))  // OK - hardcoded
  .get()

// ✅ SAFE - Use parameterized queries for dynamic values
const status = await Odac.request('status')
const result = await Odac.Mysql.run(
  'SELECT * FROM users WHERE status = ?',
  [status]  // Automatically escaped
)
```

**When to use raw()**:
- Aggregate functions: `COUNT(*)`, `SUM(price)`, `AVG(rating)`
- Database functions: `NOW()`, `CONCAT()`, `DATE_FORMAT()`
- Complex expressions that can't be built with query builder

**Never use raw() with**:
- User input from forms, URLs, or cookies
- Data from external APIs
- Any untrusted source

### Group By

```javascript
const stats = await Odac.Mysql.table('orders')
  .select('user_id', Odac.Mysql.raw('COUNT(*) as order_count'))
  .groupBy('user_id')
  .get()
```

### Complete Example

```javascript
module.exports = async function (Odac) {
  const page = await Odac.request('page') || 1
  const perPage = 20
  const search = await Odac.request('search')
  
  // Build query
  let query = Odac.Mysql.table('products')
    .select('id', 'name', 'price', 'stock')
    .where('status', 'active')
  
  // Add search filter
  if (search) {
    query = query.where('name', 'LIKE', `%${search}%`)
  }
  
  // Get products
  const products = await query
    .order('created_at', 'desc')
    .limit((page - 1) * perPage, perPage)
    .get()
  
  // Get total count
  const totalCount = await Odac.Mysql.table('products')
    .where('status', 'active')
    .rows()
  
  Odac.set({
    products: products,
    currentPage: page,
    totalPages: Math.ceil(totalCount / perPage)
  })
  
  Odac.View.set({
    skeleton: 'main',
    content: 'products.list'
  })
}
```

### Best Practices

1. **Always use the query builder** - It protects against SQL injection
2. **Never use Mysql.raw() with user input** - Only use with hardcoded values
3. **Use parameterized queries** - When using `Mysql.run()`, always pass parameters as array
4. **Use async/await** - All database methods are asynchronous
5. **Handle errors** - Wrap database calls in try/catch blocks
6. **Limit results** - Use `.limit()` to prevent loading too much data
7. **Select only needed columns** - Use `.select()` instead of selecting all columns

### Error Handling

```javascript
module.exports = async function (Odac) {
  try {
    const user = await Odac.Mysql.table('users')
      .where('id', await Odac.request('id'))
      .first()
    
    if (!user) {
      Odac.set('error', 'User not found')
    } else {
      Odac.set('user', user)
    }
  } catch (error) {
    console.error('Database error:', error)
    Odac.set('error', 'An error occurred')
  }
  
  Odac.View.set({
    skeleton: 'main',
    content: 'user.profile'
  })
}
```
