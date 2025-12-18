## 🌍 Translations (i18n)

Odac provides built-in internationalization (i18n) support, allowing you to create multi-language applications easily.

### Basic Translation

```html
<candy translate>Welcome</candy>
<candy translate>Hello World</candy>
<candy translate>Login</candy>
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

Use nested `<candy>` tags to insert dynamic values:

```html
<candy translate>Hello <candy var="user.name" /></candy>
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
<candy translate>
  <candy var="user.firstName" /> <candy var="user.lastName" />
</candy>
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
<candy translate>Hello <candy>John</candy>, how are you?</candy>
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
<candy translate>Click <a href="/help">here</a> for help</candy>
<!-- Output: Click &lt;a href="/help"&gt;here&lt;/a&gt; for help -->

<!-- Raw translation (HTML preserved) -->
<candy translate raw>Click <a href="/help">here</a> for help</candy>
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
<candy translate raw>
  Welcome <strong><candy var="user.name" /></strong>!
</candy>
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
  <a href="/"><candy translate>Home</candy></a>
  <a href="/products"><candy translate>Products</candy></a>
  <a href="/about"><candy translate>About Us</candy></a>
  <a href="/contact"><candy translate>Contact</candy></a>
</nav>
```

#### Welcome Message

```html
<div class="welcome">
  <h1>
    <candy translate>Welcome back, <candy var="user.name" />!</candy>
  </h1>
  <p>
    <candy translate>You have <candy var="notifications.length" /> new notifications</candy>
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
    <label><candy translate>Email Address</candy></label>
    <input type="email" name="email" placeholder="<candy translate>Enter your email</candy>">
  </div>
  
  <div class="form-group">
    <label><candy translate>Password</candy></label>
    <input type="password" name="password" placeholder="<candy translate>Enter your password</candy>">
  </div>
  
  <button type="submit">
    <candy translate>Login</candy>
  </button>
  
  <a href="/forgot-password">
    <candy translate>Forgot your password?</candy>
  </a>
</form>
```

#### Product Information

```html
<div class="product">
  <h2><candy var="product.name" /></h2>
  
  <p class="price">
    <candy translate>Price: $<candy var="product.price" /></candy>
  </p>
  
  <candy:if condition="product.stock > 0">
    <p class="stock">
      <candy translate><candy var="product.stock" /> units in stock</candy>
    </p>
  <candy:else>
    <p class="out-of-stock">
      <candy translate>Out of stock</candy>
    </p>
  </candy:if>
  
  <button>
    <candy translate>Add to Cart</candy>
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
<candy:if condition="errors">
  <div class="error-box">
    <candy:if condition="errors.email">
      <p><candy translate>Invalid email address</candy></p>
    </candy:if>
    
    <candy:if condition="errors.password">
      <p><candy translate>Password must be at least 8 characters</candy></p>
    </candy:if>
  </div>
</candy:if>
```

#### Rich Text with HTML

```html
<div class="notice">
  <candy translate raw>
    By clicking "Register", you agree to our 
    <a href="/terms">Terms of Service</a> and 
    <a href="/privacy">Privacy Policy</a>.
  </candy>
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
module.exports = async function(Candy) {
  // Set language from user preference
  const userLang = Candy.Auth.check() 
    ? Candy.Auth.user().language 
    : 'en'
  
  Candy.Lang.setLanguage(userLang)
  
  // Or from query parameter
  const lang = Candy.Request.get('lang') || 'en'
  Candy.Lang.setLanguage(lang)
  
  Candy.View.skeleton('main').set('content', 'home')
}
```

### Using Translation Helper in Controllers

You can also use translations in your controllers:

```javascript
module.exports = async function(Candy) {
  const message = Candy.__('Welcome back, %s!', user.name)
  
  Candy.set('message', message)
  Candy.View.skeleton('main').set('content', 'dashboard')
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
<candy:if condition="count === 1">
  <candy translate><candy var="count" /> item</candy>
<candy:else>
  <candy translate><candy var="count" /> items</candy>
</candy:if>
```

#### Date Formatting

```javascript
// Controller
const formattedDate = new Date(date).toLocaleDateString(Candy.Lang.current())
Candy.set('date', formattedDate)
```

```html
<p><candy translate>Last updated: <candy var="date" /></candy></p>
```
