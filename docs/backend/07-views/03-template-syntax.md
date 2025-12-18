## 🔧 Template Syntax Overview

Odac uses a powerful template engine to create dynamic content in view files. The engine provides a clean, HTML-like syntax for displaying variables, conditionals, loops, translations, and more.

> **Note:** Odac also supports legacy syntax (`{{ }}`, `{!! !!}`, `{{-- --}}`) for backward compatibility, but the new `<odac>` tag syntax is recommended for all new projects.

### Quick Reference

This page provides a quick overview of all available template features. For detailed documentation and examples, see the dedicated pages for each feature.

### Variables (Controller Data)

Display data passed from controllers using `Odac.set()`:

```html
<!-- HTML-safe output -->
<odac var="username" />

<!-- Raw HTML output -->
<odac var="htmlContent" raw />

<!-- String literals -->
<odac>Hello World</odac>
```

**[→ Learn more about Variables](./03-variables.md)**

### Request Data (Query Parameters)

Access URL query parameters directly:

```html
<!-- Get query parameter from URL -->
<!-- URL: /search?q=laptop -->
<odac get="q" />
<!-- Output: laptop -->
```

**Note:** `<odac get>` is for URL parameters. For controller data, use `<odac var>`.

**[→ Learn more about Request Data](./04-request-data.md)**

### Translations (i18n)

Create multi-language applications:

```html
<!-- Basic translation -->
<odac translate>Welcome</odac>

<!-- With placeholders -->
<odac translate>Hello <odac var="user.name" /></odac>

<!-- With HTML preserved -->
<odac translate raw>Click <a href="/help">here</a></odac>
```

**[→ Learn more about Translations](./07-translations.md)**

### Comments

Two types of comments for different purposes:

```html
<!--odac Backend comment (not rendered) -->

<!--odac
  Multi-line backend comment
  Won't appear in output
odac-->

<!-- Regular HTML comment (rendered) -->
```

**[→ Learn more about Comments](./09-comments.md)**

### Conditionals

Show or hide content based on conditions:

```html
<odac:if condition="user.isAdmin">
  <p>Admin panel</p>
<odac:elseif condition="user.isModerator">
  <p>Moderator panel</p>
<odac:else>
  <p>User panel</p>
</odac:if>
```

**[→ Learn more about Conditionals](./05-conditionals.md)**

### Loops

Iterate over arrays and objects:

```html
<!-- For loop -->
<odac:for in="users" key="index" value="user">
  <div><odac var="user.name" /></div>
</odac:for>

<!-- While loop -->
<odac:while condition="counter < 10">
  <p><odac var="counter" /></p>
</odac:while>

<!-- Loop control -->
<odac:break />
<odac:continue />
```

**[→ Learn more about Loops](./06-loops.md)**

### Backend JavaScript

Execute JavaScript on the server during template rendering:

```html
<script:odac>
  // Runs on SERVER before HTML is sent
  let total = 0;
  for (let item of cart) {
    total += item.price * item.quantity;
  }
</script:odac>

<p>Total: $<odac var="total" /></p>
```

**[→ Learn more about Backend JavaScript](./08-backend-javascript.md)**

### Accessing the Odac Object

Full access to the Odac object in templates:

```html
<odac:if condition="Odac.Auth.check()">
  <p>User: <odac var="Odac.Auth.user().name" /></p>
</odac:if>

<p>URL: <odac var="Odac.Request.url" /></p>
```

### Complete Syntax Reference

| Feature | Syntax | Documentation |
|---------|--------|---------------|
| Variable (Controller) | `<odac var="x" />` | [Variables](./03-variables.md) |
| Raw HTML | `<odac var="x" raw />` | [Variables](./03-variables.md) |
| String | `<odac>text</odac>` | [Variables](./03-variables.md) |
| Query Parameter | `<odac get="key" />` | [Request Data](./04-request-data.md) |
| Translation | `<odac translate>key</odac>` | [Translations](./07-translations.md) |
| Translation Raw | `<odac translate raw>key</odac>` | [Translations](./07-translations.md) |
| If | `<odac:if condition="x">` | [Conditionals](./05-conditionals.md) |
| Elseif | `<odac:elseif condition="x">` | [Conditionals](./05-conditionals.md) |
| Else | `<odac:else>` | [Conditionals](./05-conditionals.md) |
| For | `<odac:for in="x" value="item">` | [Loops](./06-loops.md) |
| While | `<odac:while condition="x">` | [Loops](./06-loops.md) |
| Break | `<odac:break />` | [Loops](./06-loops.md) |
| Continue | `<odac:continue />` | [Loops](./06-loops.md) |
| JavaScript | `<script:odac>...</script:odac>` | [Backend JavaScript](./08-backend-javascript.md) |
| Comment | `<!--odac ... odac-->` | [Comments](./09-comments.md) |

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

**Note:** The new `<odac>` tag syntax is recommended for all new projects.

