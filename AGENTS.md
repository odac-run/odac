# AGENTS Guidelines for This Repository

This document provides a comprehensive guide for developers working on the Odac source code. It covers the project's architecture, class system, directory structure, and usage patterns.

## 1. Project Overview

Odac is a toolkit for building and deploying web applications, consisting of two main parts:
1.  **A Core System**: A background service that manages servers (web, mail, etc.), process monitoring, and provides a command-line interface (CLI).
2.  **A Web Framework**: A lightweight framework for building the actual web applications that are served by the core system.

## 2. Core Architecture (`core/Odac.js`)

The heart of the core system is a service container implemented in `core/Odac.js`. This module creates a global singleton object named `Odac`.

### Service Container

The `Odac` object acts as a service locator and dependency injection container. It can dynamically load and cache modules (services) from different parts of the application.

-   **Registration and Resolution**: Modules are registered with a key and can be resolved (retrieved) using that key. The container handles the instantiation of classes and can cache them as singletons.
-   **Module Loaders**: It has dedicated methods for loading modules from specific directories:
    -   `Odac.core(name)`: Loads a module from the `core/` directory.
    -   `Odac.cli(name)`: Loads a module from the `cli/src/` directory.
    -   `Odac.server(name)`: Loads a module from the `server/src/` directory.
    -   `Odac.watchdog(name)`: Loads a module from the `watchdog/src` directory.

**Example Usage:**
To access the application's configuration manager (defined in `core/Config.js`), you would use:
```javascript
const config = Odac.core('Config');
```

## 3. Web Framework Architecture (`framework/src/Odac.js`)

The web framework provides the tools to build a website or API. Its main entry point is `framework/src/Odac.js`. Unlike the core `Odac` object, the framework creates a **request-specific instance** for every incoming HTTP request.

### Request-Specific Context

When a web request hits the server, the framework's `instance()` method is called to create a `_odac` object. This object is a "context" that holds all the necessary components for handling that specific request.

This context object includes:
-   `Request`: An object representing the incoming HTTP request.
-   `View`: A view renderer for HTML pages.
-   `Auth`: An authentication manager.
-   `Token`: A CSRF token handler.
-   `Lang`: An internationalization (i18n) helper.
-   `Mysql`: A database connection handler.

It also attaches numerous helper functions to the context object for convenience within a controller, such as:
-   `return(data)`: Ends the request and sends data back to the client.
-   `direct(url)`: Redirects the client to a new URL.
-   `cookie(key, value)`: Sets a cookie.
-   `validator()`: Creates a new input validator.

This context object is passed to the controller function that handles the route.

## 4. Directory Structure Guide

-   `bin/`: Executable scripts for the CLI (`odac`).
-   `cli/`: Source code for the CLI. The main logic is in `cli/src/Cli.js`.
-   `core/`: Core application logic and shared modules (e.g., `Config.js`, `Commands.js`).
-   `framework/`: The source code for the web framework.
-   `server/`: Low-level server implementations (HTTP, Mail, DNS, SSL).
-   `web/`: **This is a template for a user's website.** When a new site is created with Odac, this directory is copied to serve as the starting point.
    -   `web/index.js`: The entry point for a web application. It initializes the framework.
    -   `web/route/`: Contains route definition files.
    -   `web/controller/`: Contains controller files.
    -   `web/view/`: (Assumed) Would contain HTML templates for the `View` engine.
    -   `web/config.json`: Application-specific configuration.
-   `test/`: Project test files.
-   `watchdog/`: A process monitor that ensures the main Odac server stays running.

## 5. Using the CLI (`bin/odac`)

The `odac` command is the main tool for managing the Odac server.

-   `odac`: Shows the server status (online/offline, uptime, etc.) and lists available commands.
-   `odac start`: (Inferred) Starts the Odac server daemon.
-   `odac stop`: (Inferred) Stops the server.
-   `odac restart`: (Inferred) Restarts the server.
-   `odac monitor`: Opens an interactive terminal UI to monitor the status of all hosted websites and services.
-   `odac debug`: Opens an interactive UI to view live logs from different core modules.
-   `odac help <command>`: Shows detailed help for a specific command.

The commands are defined in `core/Commands.js` and implemented in various modules, primarily `cli/src/Cli.js`.
