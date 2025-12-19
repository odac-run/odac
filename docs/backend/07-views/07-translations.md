## 🌍 Translations (i18n)

Odac provides built-in internationalization (i18n) support, allowing you to create multi-language applications easily.

### Basic Translation

```html
<odac translate>Welcome</odac>
<odac translate>Hello World</odac>
<odac translate>Login</odac>
```

The text inside the tag is used as the translation key. Odac looks up this key in your locale files.

### Translation Files

Translation files are stored in the `locale/` directory:

```
locale/
├── en.json
├── tr.json
└── de.json
```

**Example: `locale/en.json`**
```json
{
  "Welcome": "Welcome",
  "Hello World": "Hello World",
  "Login": "Login",
  "Logout": "Logout"
}
```

**Example: `locale/tr.json`**
```json
{
  "Welcome": "Hoş Geldiniz",
  "Hello World": "Merhaba Dünya",
  "Login": "Giriş Yap",
  "Logout": "Çıkış Yap"
}
```

### Translations with Placeholders

Use nested `<odac>` tags to insert dynamic values:

```html
<odac translate>Hello <odac var="user.name" /></odac>
```

**How it works:**
1. The content becomes: `Hello %s1`
2. Odac looks up this key in the locale file
3. `%s1` is replaced with the actual value

**Locale file:**
```json
{
  "Hello %s1": "Hello %s1",
  "Hello %s1": "Merhaba %s1"
}
```

### Multiple Placeholders

```html
<odac translate>
  <odac var="user.firstName" /> <odac var="user.lastName" />
</odac>
```

This creates the key `%s1 %s2` and replaces both placeholders.

**Locale file:**
```json
{
  "%s1 %s2": "%s1 %s2",
  "%s1 %s2": "%s1 %s2"
}
```

### String Literals in Translations

```html
<odac translate>Hello <odac>John</odac>, how are you?</odac>
```

**Locale file:**
```json
{
  "Hello %s1, how are you?": "Hello %s1, how are you?",
  "Hello %s1, how are you?": "Merhaba %s1, nasılsın?"
}
```

### Raw HTML in Translations

By default, translations are HTML-escaped for security. Use `raw` attribute when your translation contains HTML:

```html
<!-- Normal translation (HTML will be encoded) -->
<odac translate>Click <a href="/help">here</a> for help</odac>
<!-- Output: Click &lt;a href="/help"&gt;here&lt;/a&gt; for help -->

<!-- Raw translation (HTML preserved) -->
<odac translate raw>Click <a href="/help">here</a> for help</odac>
<!-- Output: Click <a href="/help">here</a> for help -->
```

**Locale file:**
```json
{
  "Click <a href=\"/help\">here</a> for help": "Click <a href=\"/help\">here</a> for help",
  "Click <a href=\"/help\">here</a> for help": "Yardım için <a href=\"/help\">buraya</a> tıklayın"
}
```

### Raw Translations with Placeholders

```html
<odac translate raw>
  Welcome <strong><odac var="user.name" /></strong>!
</odac>
```

**Locale file:**
```json
{
  "Welcome <strong>%s1</strong>!": "Welcome <strong>%s1</strong>!",
  "Welcome <strong>%s1</strong>!": "Hoş geldin <strong>%s1</strong>!"
}
```

### Practical Examples

#### Navigation Menu

```html
<nav>
  <a href="/"><odac translate>Home</odac></a>
  <a href="/products"><odac translate>Products</odac></a>
  <a href="/about"><odac translate>About Us</odac></a>
  <a href="/contact"><odac translate>Contact</odac></a>
</nav>
```

#### Welcome Message

```html
<div class="welcome">
  <h1>
    <odac translate>Welcome back, <odac var="user.name" />!</odac>
  </h1>
  <p>
    <odac translate>You have <odac var="notifications.length" /> new notifications</odac>
  </p>
</div>
```

**Locale file:**
```json
{
  "Welcome back, %s1!": "Welcome back, %s1!",
  "Welcome back, %s1!": "Tekrar hoş geldin, %s1!",
  "You have %s1 new notifications": "You have %s1 new notifications",
  "You have %s1 new notifications": "%s1 yeni bildiriminiz var"
}
```

#### Form Labels and Buttons

```html
<form>
  <div class="form-group">
    <label><odac translate>Email Address</odac></label>
    <input type="email" name="email" placeholder="<odac translate>Enter your email</odac>">
  </div>
  
  <div class="form-group">
    <label><odac translate>Password</odac></label>
    <input type="password" name="password" placeholder="<odac translate>Enter your password</odac>">
  </div>
  
  <button type="submit">
    <odac translate>Login</odac>
  </button>
  
  <a href="/forgot-password">
    <odac translate>Forgot your password?</odac>
  </a>
</form>
```

#### Product Information

```html
<div class="product">
  <h2><odac var="product.name" /></h2>
  
  <p class="price">
    <odac translate>Price: $<odac var="product.price" /></odac>
  </p>
  
  <odac:if condition="product.stock > 0">
    <p class="stock">
      <odac translate><odac var="product.stock" /> units in stock</odac>
    </p>
  <odac:else>
    <p class="out-of-stock">
      <odac translate>Out of stock</odac>
    </p>
  </odac:if>
  
  <button>
    <odac translate>Add to Cart</odac>
  </button>
</div>
```

**Locale file:**
```json
{
  "Price: $%s1": "Price: $%s1",
  "Price: $%s1": "Fiyat: $%s1",
  "%s1 units in stock": "%s1 units in stock",
  "%s1 units in stock": "Stokta %s1 adet",
  "Out of stock": "Out of stock",
  "Out of stock": "Stokta yok",
  "Add to Cart": "Add to Cart",
  "Add to Cart": "Sepete Ekle"
}
```

#### Error Messages

```html
<odac:if condition="errors">
  <div class="error-box">
    <odac:if condition="errors.email">
      <p><odac translate>Invalid email address</odac></p>
    </odac:if>
    
    <odac:if condition="errors.password">
      <p><odac translate>Password must be at least 8 characters</odac></p>
    </odac:if>
  </div>
</odac:if>
```

#### Rich Text with HTML

```html
<div class="notice">
  <odac translate raw>
    By clicking "Register", you agree to our 
    <a href="/terms">Terms of Service</a> and 
    <a href="/privacy">Privacy Policy</a>.
  </odac>
</div>
```

**Locale file:**
```json
{
  "By clicking \"Register\", you agree to our <a href=\"/terms\">Terms of Service</a> and <a href=\"/privacy\">Privacy Policy</a>.": "By clicking \"Register\", you agree to our <a href=\"/terms\">Terms of Service</a> and <a href=\"/privacy\">Privacy Policy</a>.",
  "By clicking \"Register\", you agree to our <a href=\"/terms\">Terms of Service</a> and <a href=\"/privacy\">Privacy Policy</a>.": "\"Kayıt Ol\" butonuna tıklayarak <a href=\"/terms\">Hizmet Şartlarımızı</a> ve <a href=\"/privacy\">Gizlilik Politikamızı</a> kabul etmiş olursunuz."
}
```

### Setting the Language

The language is typically set based on user preference or browser settings. You can set it in your controller:

```javascript
// Controller
module.exports = async function(Odac) {
  // Set language from user preference
  const userLang = Odac.Auth.check() 
    ? Odac.Auth.user().language 
    : 'en'
  
  Odac.Lang.setLanguage(userLang)
  
  // Or from query parameter
  const lang = Odac.Request.get('lang') || 'en'
  Odac.Lang.setLanguage(lang)
  
  Odac.View.skeleton('main').set('content', 'home')
}
```

### Using Translation Helper in Controllers

You can also use translations in your controllers:

```javascript
module.exports = async function(Odac) {
  const message = Odac.__('Welcome back, %s!', user.name)
  
  Odac.set('message', message)
  Odac.View.skeleton('main').set('content', 'dashboard')
}
```

### Best Practices

1. **Use descriptive keys**: Make translation keys meaningful and context-aware
2. **Keep keys consistent**: Use the same key for the same text across your app
3. **Organize locale files**: Group related translations together
4. **Escape HTML carefully**: Only use `raw` with trusted content
5. **Test all languages**: Ensure translations work correctly in all supported languages
6. **Handle missing translations**: Provide fallback values

**Good locale structure:**
```json
{
  "nav.home": "Home",
  "nav.products": "Products",
  "nav.about": "About",
  "form.email": "Email Address",
  "form.password": "Password",
  "form.submit": "Submit",
  "error.invalid_email": "Invalid email address",
  "error.required_field": "This field is required"
}
```

**Security Warning:**
- Never use `raw` with user-generated content
- Always validate and sanitize user input before translation
- Be careful with HTML in translation strings

### Common Patterns

#### Pluralization

```html
<odac:if condition="count === 1">
  <odac translate><odac var="count" /> item</odac>
<odac:else>
  <odac translate><odac var="count" /> items</odac>
</odac:if>
```

#### Date Formatting

```javascript
// Controller
const formattedDate = new Date(date).toLocaleDateString(Odac.Lang.current())
Odac.set('date', formattedDate)
```

```html
<p><odac translate>Last updated: <odac var="date" /></odac></p>
```
