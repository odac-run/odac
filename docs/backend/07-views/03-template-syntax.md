## 🔧 Template Syntax Overview

Odac uses a powerful template engine to create dynamic content in view files. The engine provides a clean, HTML-like syntax for displaying variables, conditionals, loops, translations, and more.

> **Note:** Odac also supports legacy syntax (`{{ }}`, `{!! !!}`, `{{-- --}}`) for backward compatibility, but the new `<candy>` tag syntax is recommended for all new projects.

### Quick Reference

This page provides a quick overview of all available template features. For detailed documentation and examples, see the dedicated pages for each feature.

### Variables (Controller Data)

Display data passed from controllers using `Candy.set()`:

```html
<!-- HTML-safe output -->
<candy var="username" />

<!-- Raw HTML output -->
<candy var="htmlContent" raw />

<!-- String literals -->
<candy>Hello World</candy>
```

**[→ Learn more about Variables](./03-variables.md)**

### Request Data (Query Parameters)

Access URL query parameters directly:

```html
<!-- Get query parameter from URL -->
<!-- URL: /search?q=laptop -->
<candy get="q" />
<!-- Output: laptop -->
```

**Note:** `<candy get>` is for URL parameters. For controller data, use `<candy var>`.

**[→ Learn more about Request Data](./04-request-data.md)**

### Translations (i18n)

Create multi-language applications:

```html
<!-- Basic translation -->
<candy translate>Welcome</candy>

<!-- With placeholders -->
<candy translate>Hello <candy var="user.name" /></candy>

<!-- With HTML preserved -->
<candy translate raw>Click <a href="/help">here</a></candy>
```

**[→ Learn more about Translations](./07-translations.md)**

### Comments

Two types of comments for different purposes:

```html
<!--candy Backend comment (not rendered) -->

<!--candy
  Multi-line backend comment
  Won't appear in output
candy-->

<!-- Regular HTML comment (rendered) -->
```

**[→ Learn more about Comments](./09-comments.md)**

### Conditionals

Show or hide content based on conditions:

```html
<candy:if condition="user.isAdmin">
  <p>Admin panel</p>
<candy:elseif condition="user.isModerator">
  <p>Moderator panel</p>
<candy:else>
  <p>User panel</p>
</candy:if>
```

**[→ Learn more about Conditionals](./05-conditionals.md)**

### Loops

Iterate over arrays and objects:

```html
<!-- For loop -->
<candy:for in="users" key="index" value="user">
  <div><candy var="user.name" /></div>
</candy:for>

<!-- While loop -->
<candy:while condition="counter < 10">
  <p><candy var="counter" /></p>
</candy:while>

<!-- Loop control -->
<candy:break />
<candy:continue />
```

**[→ Learn more about Loops](./06-loops.md)**

### Backend JavaScript

Execute JavaScript on the server during template rendering:

```html
<script:candy>
  // Runs on SERVER before HTML is sent
  let total = 0;
  for (let item of cart) {
    total += item.price * item.quantity;
  }
</script:candy>

<p>Total: $<candy var="total" /></p>
```

**[→ Learn more about Backend JavaScript](./08-backend-javascript.md)**

### Accessing the Candy Object

Full access to the Candy object in templates:

```html
<candy:if condition="Candy.Auth.check()">
  <p>User: <candy var="Candy.Auth.user().name" /></p>
</candy:if>

<p>URL: <candy var="Candy.Request.url" /></p>
```

### Complete Syntax Reference

| Feature | Syntax | Documentation |
|---------|--------|---------------|
| Variable (Controller) | `<candy var="x" />` | [Variables](./03-variables.md) |
| Raw HTML | `<candy var="x" raw />` | [Variables](./03-variables.md) |
| String | `<candy>text</candy>` | [Variables](./03-variables.md) |
| Query Parameter | `<candy get="key" />` | [Request Data](./04-request-data.md) |
| Translation | `<candy translate>key</candy>` | [Translations](./07-translations.md) |
| Translation Raw | `<candy translate raw>key</candy>` | [Translations](./07-translations.md) |
| If | `<candy:if condition="x">` | [Conditionals](./05-conditionals.md) |
| Elseif | `<candy:elseif condition="x">` | [Conditionals](./05-conditionals.md) |
| Else | `<candy:else>` | [Conditionals](./05-conditionals.md) |
| For | `<candy:for in="x" value="item">` | [Loops](./06-loops.md) |
| While | `<candy:while condition="x">` | [Loops](./06-loops.md) |
| Break | `<candy:break />` | [Loops](./06-loops.md) |
| Continue | `<candy:continue />` | [Loops](./06-loops.md) |
| JavaScript | `<script:candy>...</script:candy>` | [Backend JavaScript](./08-backend-javascript.md) |
| Comment | `<!--candy ... candy-->` | [Comments](./09-comments.md) |

### Legacy Syntax

Odac also supports legacy syntax for backward compatibility:

```html
<!-- Variable output -->
{{ username }}

<!-- Raw HTML -->
{!! htmlContent !!}

<!-- Comments -->
{{-- This is a comment --}}
```

**Note:** The new `<candy>` tag syntax is recommended for all new projects.

