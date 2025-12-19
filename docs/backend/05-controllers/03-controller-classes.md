## 🎓 Controller Classes

While simple function exports work great for basic controllers, you can also organize your code using classes. This is especially useful when you want to share logic between multiple methods or keep related functionality together.

#### Creating a Controller Class

A controller class receives the `Odac` object in its constructor, giving you access to all services throughout your class methods:

```javascript
// controller/User.js
class User {
  constructor(Odac) {
    this.Odac = Odac
  }

  async getProfile() {
    const user = await this.Odac.Auth.user()
    return this.Odac.return({
      success: true,
      user: user
    })
  }

  async updateProfile() {
    const validator = this.Odac.validator()
    validator.post('name').required().min(3)
    validator.post('email').required().email()

    if (await validator.error()) {
      return validator.result()
    }

    const name = await this.Odac.request('name')
    const email = await this.Odac.request('email')

    // Update user in database
    await this.Odac.Mysql.query('UPDATE users SET name = ?, email = ? WHERE id = ?', [
      name,
      email,
      this.Odac.Auth.user().id
    ])

    return this.Odac.return({
      success: true,
      message: 'Profile updated successfully'
    })
  }
}

module.exports = User
```

#### Using Controller Classes in Routes

Once you've created a controller class, you can use it in your routes just like any other controller:

```javascript
// route/www.js
Odac.Route.buff = 'www'

// Access class methods using dot notation
Odac.Route.get('/profile', 'User.getProfile')
Odac.Route.post('/profile/update', 'User.updateProfile')
```

#### Accessing Classes in Controllers

Controller classes are automatically instantiated for each request and attached to the `Odac` object. You can access them from any controller:

```javascript
module.exports = async function (Odac) {
  // Access your User class
  const profile = await Odac.User.getProfile()
  
  return Odac.return(profile)
}
```

#### Benefits of Controller Classes

- **Organization**: Group related methods together
- **Reusability**: Share logic between different routes
- **Maintainability**: Easier to manage complex controllers
- **Context**: The `Odac` object is always available via `this.Odac`

#### Class vs Function Controllers

Both approaches work perfectly fine. Use what makes sense for your project:

- **Functions**: Great for simple, single-purpose controllers
- **Classes**: Better for complex logic with multiple related methods

The framework automatically detects whether your export is a class or a function and handles it accordingly.
