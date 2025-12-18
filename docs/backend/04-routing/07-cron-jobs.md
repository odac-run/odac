# Cron Jobs

The Odac framework provides a built-in cron system for running automated tasks. This system checks every minute and executes jobs based on specified conditions.

## Basic Usage

Use the `Odac.Route.cron()` method to define cron jobs:

```javascript
// With controller file
Odac.Route.cron('backup').everyDay(1) // Runs every day

// With direct function
Odac.Route.cron(() => {
  console.log('Task executed!')
}).everyHour(2) // Runs every 2 hours
```

## Controller Files

Controller files for cron jobs are created in the `controller/cron/` directory:

```javascript
// controller/cron/backup.js
module.exports = () => {
  console.log('Backup process started')
  // Backup code...
}
```

For module-based organization:

```javascript
// controller/admin/cron/cleanup.js
module.exports = () => {
  console.log('Cleanup process')
}

// Usage
Odac.Route.cron('admin.cleanup').everyDay(1)
```

## Direct Function Usage

You can also define cron jobs with inline functions:

```javascript
// Simple inline function
Odac.Route.cron(() => console.log('Simple task running')).everyMinute(5)

// Async function
Odac.Route.cron(async () => {
  const data = await fetchSomeData()
  console.log('Async task completed', data)
}).everyHour(1)

// Function with parameters
const cleanupTask = (directory) => {
  console.log(`Cleaning up ${directory}`)
  // Cleanup logic...
}

Odac.Route.cron(() => cleanupTask('/tmp')).everyDay(1)
```

## Time Conditions

### Specific Time Values

```javascript
Odac.Route.cron('task')

// Minute (0-59)
.minute(30) // At 30th minute

// Hour (0-23)
.hour(14) // At 14:00

// Day (1-31)
.day(15) // On the 15th of the month

// Week day (0-6, 0=Sunday)
.weekDay(1) // On Monday

// Month (1-12)
.month(6) // In June

// Year
.year(2024) // In 2024

// Year day (1-365)
.yearDay(100) // On the 100th day of the year
```

### Periodic Execution

```javascript
Odac.Route.cron('periodic')

// Every N minutes
.everyMinute(5) // Every 5 minutes

// Every N hours
.everyHour(3) // Every 3 hours

// Every N days
.everyDay(2) // Every 2 days

// Every N weeks
.everyWeekDay(1) // Every week

// Every N months
.everyMonth(2) // Every 2 months

// Every N years
.everyYear(1) // Every year
```

## Combination Usage

You can combine multiple conditions:

```javascript
// Every day at 14:30
Odac.Route.cron('daily-report')
  .hour(14)
  .minute(30)

// Mondays at 09:00
Odac.Route.cron('weekly-task')
  .weekDay(1)
  .hour(9)
  .minute(0)

// First day of every month at midnight
Odac.Route.cron('monthly-cleanup')
  .day(1)
  .hour(0)
  .minute(0)
```

## Important Notes

- The cron system checks every minute
- Jobs run at the first suitable time after their last update
- If the same job is defined multiple times, the last definition takes precedence
- Controller files are re-required on each execution
- Inline functions are stored in memory and executed directly
- If a job fails, it stops but the system continues